package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/sagernet/sing/common/control"
)

var (
	logger *slog.Logger
)

type Config struct {
	IsServer bool   `json:"is_server"`
	LogLevel string `json:"log_level"`

	Listen    string `json:"listen"`
	Forward   string `json:"forward"`
	Interface string `json:"iface"`

	Timeout     int      `json:"timeout"`
	ConnTimeout int      `json:"conn_timeout"`
	GroupLive   int      `json:"group_live"`
	Fec         []uint16 `json:"fec"`
	GCInterval  int      `json:"gc_interval"`

	encoders []reedsolomon.Encoder
}

func udp_addr(addr string) *net.UDPAddr {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)

	if err != nil {
		panic(err)
	}
	return udpaddr
}

type WriteConn struct {
	*net.UDPConn
	*net.UDPAddr
}

func (s *WriteConn) Write(b []byte) (n int, err error) {
	if s.UDPAddr != nil {
		return s.UDPConn.WriteToUDP(b, s.UDPAddr)
	} else if s.UDPConn != nil {
		return s.UDPConn.Write(b)
	}
	return 0, nil
}

func encode(enc chan []byte, out WriteConn, config *Config) {
	var (
		shards   [][]byte           = make([][]byte, 0)
		id       uint32             = rand.Uint32()
		is_final bool               = false
		ctx      context.Context    = context.Background()
		cancel   context.CancelFunc = func() {}
	)

	for {
		select {
		case b, ok := <-enc:
			if !ok {
				cancel()
				return
			}

			shards = append(shards, b)
			b = encode_packet(shards, id, len(shards)-1)

			out.Write(b)

			is_final = len(shards) == len(config.Fec)
		case <-ctx.Done():
			cancel()
			ctx = context.Background()

			is_final = true
		}

		logger.Debug(fmt.Sprintf("encode: %x", id))

		if is_final {
			parity := final(shards, id, config.Fec[len(shards)-1], config.encoders[len(shards)-1])

			for i := range parity {
				out.Write(parity[i])
			}

			shards = make([][]byte, 0)
			id = rand.Uint32()

			cancel()
			ctx = context.Background()
		} else if len(shards) == 1 {
			ctx, cancel = context.WithTimeout(context.Background(), time.Duration(config.Timeout)*time.Millisecond)
		}
	}
}

func decode(dec chan []byte, out WriteConn, config *Config) {
	groups := make(map[uint32]*DecGroup)
	last_gc := time.Now()

	for {
		// GC
		now := time.Now()
		if last_gc.Add(time.Duration(config.GCInterval) * time.Millisecond).Before(now) {
			for i := range groups {
				if groups[i].t.Add(time.Duration(config.GroupLive) * time.Millisecond).Before(now) {
					delete(groups, i)
				}
			}

			last_gc = now
		}

		b, ok := <-dec
		if !ok {
			return
		}

		id, err := read_id(b)
		if err != nil {
			logger.Warn(err.Error())
			continue
		}

		logger.Debug(fmt.Sprintf("decode: %x", id))

		if _, ok := groups[id]; !ok {
			groups[id] = &DecGroup{t: time.Now()}
		}
		group := groups[id]

		if group.finished {
			continue
		}

		b, err = group.decode_packet(b, config.Fec)
		if err != nil {
			logger.Warn(err.Error())
			continue
		}

		if b != nil {
			out.Write(b)
			continue
		}

		if group.content_n != 0 {
			if group.content_m == group.content_n {
				logger.Debug(fmt.Sprintf("finished: %x", id))
				group.finished = true
				continue
			}

			if data, err := group.reconstruct(config.Fec[group.content_n-1], config.encoders[group.content_n-1]); err == nil {
				if data == nil {
					continue
				}

				logger.Debug(fmt.Sprintf("reconstructed: %x", id))
				for i := range data {
					if i >= len(group.sent) || !group.sent[i] {
						out.Write(data[i])
					}
				}
				group.finished = true
			} else {
				logger.Warn(err.Error())
			}
		}
	}
}

type Conn struct {
	c           chan []byte
	last_active time.Time
}

func main() {
	configFile, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}

	var config Config

	err = json.NewDecoder(configFile).Decode(&config)
	if err != nil {
		panic(err)
	}

	if config.Timeout == 0 {
		config.Timeout = 15
	}
	if config.ConnTimeout == 0 {
		config.ConnTimeout = 30000
	}
	if config.GroupLive == 0 {
		config.GroupLive = 5000
	}
	if config.GCInterval == 0 {
		config.GCInterval = 5000
	}

	level := slog.LevelWarn
	if config.LogLevel != "" {
		level.UnmarshalText([]byte(config.LogLevel))
	}
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	listen, err := net.ListenUDP("udp", udp_addr(config.Listen))
	if err != nil {
		panic(err)
	}

	bind := func(conn *net.UDPConn) error {
		return nil
	}

	var laddr *net.UDPAddr = nil

	if config.Interface != "" {
		iface, err := net.InterfaceByName(config.Interface)
		if err != nil {
			panic(err)
		}

		addrs, err := iface.Addrs()
		if err != nil {
			panic(err)
		}

		for i := range addrs {
			if addrs[i].(*net.IPNet).IP.IsGlobalUnicast() {
				laddr = &net.UDPAddr{IP: addrs[i].(*net.IPNet).IP}
				logger.Debug("laddr: " + laddr.String())
				break
			}
		}

		if laddr == nil {
			panic(fmt.Errorf("no valid addrs found on the interface"))
		}

		bindfunc := control.BindToInterface(control.DefaultInterfaceFinder(), config.Interface, -1)
		bind = func(conn *net.UDPConn) error {
			rawconn, err := conn.SyscallConn()
			if err != nil {
				panic(err)
			}

			return bindfunc("udp", "", rawconn)
		}
	}

	config.encoders = make([]reedsolomon.Encoder, len(config.Fec))
	for i := range config.encoders {
		config.encoders[i], err = reedsolomon.New(i+1, int(config.Fec[i]))
		if err != nil {
			panic(err)
		}
	}

	conns := make(map[netip.AddrPort]*Conn)
	conns_lock := &sync.Mutex{}

	// GC
	go func() {
		for {
			now := time.Now()

			for i := range conns {
				if conns[i].last_active.Add(time.Duration(config.ConnTimeout) * time.Millisecond).Before(now) {
					close(conns[i].c)
					delete(conns, i)
				}
			}

			time.Sleep(time.Duration(config.GCInterval) * time.Millisecond)
		}
	}()

	for {
		b := make([]byte, buf_size)
		n, from, err := listen.ReadFromUDP(b)

		if err != nil {
			panic(err)
		}

		var conn *Conn
		conns_lock.Lock()
		if value, ok := conns[from.AddrPort()]; !ok {
			logger.Info("new connection: " + from.String())

			conn = &Conn{c: make(chan []byte), last_active: time.Now()}
			conns[from.AddrPort()] = conn

			remote := make(chan []byte)

			go func() {
				forward, err := net.DialUDP("udp", laddr, udp_addr(config.Forward))

				defer func() {
					close(remote)

					conns_lock.Lock()
					close(conn.c)
					delete(conns, from.AddrPort())
					conns_lock.Unlock()
				}()

				if err != nil {
					logger.Warn(err.Error())
					return
				}

				bind(forward)

				if config.IsServer {
					go encode(remote, WriteConn{listen, from}, &config)
					go decode(conn.c, WriteConn{forward, nil}, &config)
				} else {
					go encode(conn.c, WriteConn{forward, nil}, &config)
					go decode(remote, WriteConn{listen, from}, &config)
				}

				for {
					forward.SetReadDeadline(time.Now().Add(time.Duration(config.ConnTimeout) * time.Millisecond))

					b := make([]byte, buf_size)
					n, err := forward.Read(b)

					if err != nil {
						if e, ok := err.(net.Error); ok && e.Timeout() {
							logger.Info(err.Error())
						} else {
							logger.Warn(err.Error())
						}
						return
					}

					conn.last_active = time.Now()

					remote <- b[:n]
				}
			}()
		} else {
			conn = value
		}
		conn.c <- b[:n]
		conns_lock.Unlock()
	}
}

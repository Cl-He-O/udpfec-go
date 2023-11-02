package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/sagernet/sing/common/control"
)

type Config struct {
	Listen      string   `json:"listen"`
	Forward     string   `json:"forward"`
	Interface   string   `json:"iface"`
	IsServer    bool     `json:"is_server"`
	Timeout     int      `json:"timeout"`
	ConnTimeout int      `json:"conn_timeout"`
	GroupLive   int      `json:"group_live"`
	Fec         []uint16 `json:"fec"`

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
		shards   [][]byte = make([][]byte, 0)
		id       uint32   = rand.Uint32()
		is_final bool
		ctx      context.Context = context.Background()
		cancel   context.CancelFunc
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

		//fmt.Printf("%d\n", id)

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
	groups := sync.Map{}

	// GC
	go func() {
		for {
			now := time.Now()

			groups.Range(func(key, value any) bool {
				if value.(*DecGroup).t.Add(time.Duration(config.GroupLive) * time.Millisecond).Before(now) {
					groups.Delete(key)
				}

				return true
			})

			time.Sleep(time.Second)
		}
	}()

	for {
		b, ok := <-dec
		if !ok {
			return
		}

		id, err := read_id(b)
		if err != nil {
			slog.Warn(err.Error())
			continue
		}

		//fmt.Printf("id: %x\n", id)

		if _, ok := groups.Load(id); !ok {
			groups.Store(id, &DecGroup{t: time.Now()})
		}
		value, _ := groups.Load(id)
		group := value.(*DecGroup)

		if group.finished {
			continue
		}

		b, err = group.decode_packet(b, config.Fec)
		if err != nil {
			slog.Warn(err.Error())
			continue
		}

		if b != nil {
			out.Write(b)
			continue
		}

		if group.content_n != 0 {
			if group.content_m == group.content_n {
				//fmt.Printf("finished group %x\n", id)
				group.finished = true
				continue
			}

			if data, err := group.reconstruct(config.Fec[group.content_n-1], config.encoders[group.content_n-1]); err == nil {
				if data == nil {
					continue
				}

				//fmt.Printf("reconstructed group %x\n", id)
				for i := range data {
					if i >= len(group.sent) || !group.sent[i] {
						out.Write(data[i])
					}
				}
				group.finished = true
			} else {
				slog.Warn(err.Error())
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
		laddr = &net.UDPAddr{IP: addrs[0].(*net.IPNet).IP}

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

	conns := sync.Map{}

	// GC
	go func() {
		for {
			now := time.Now()

			conns.Range(func(key, value any) bool {
				if value.(*Conn).last_active.Add(time.Duration(config.ConnTimeout) * time.Millisecond).Before(now) {
					value.(*Conn).c = nil
					conns.Delete(key)
				}

				return true
			})

			time.Sleep(time.Second)
		}
	}()

	for {
		b := make([]byte, buf_size)
		n, from, err := listen.ReadFromUDP(b)

		if err != nil {
			panic(err)
		}

		if value, ok := conns.Load(from.AddrPort()); !ok {
			slog.Info(fmt.Sprintf("new connection from %s", from))

			conn := &Conn{make(chan []byte), time.Now()}
			conns.Store(from.AddrPort(), conn)

			remote := make(chan []byte)

			go func() {
				forward, err := net.DialUDP("udp", laddr, udp_addr(config.Forward))

				defer func() {
					remote = nil
					conn.c = nil
					conns.Delete(from.AddrPort())
				}()

				if err != nil {
					slog.Warn(err.Error())
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
						slog.Warn(err.Error())

						return
					}

					conn.last_active = time.Now()
					remote <- b[:n]
				}
			}()

			conn.c <- b[:n]
		} else {
			value.(*Conn).c <- b[:n]
		}
	}
}

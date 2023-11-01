package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/klauspost/reedsolomon"
)

type Config struct {
	Listen    string   `json:"listen"`
	Forward   string   `json:"forward"`
	IsServer  bool     `json:"is_server"`
	Timeout   int      `json:"timeout"`
	GroupLive int      `json:"group_live"`
	Fec       []uint16 `json:"fec"`
}

func udpAddr(addr string) *net.UDPAddr {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)

	if err != nil {
		panic(err)
	}
	return udpaddr
}

func encode(enc chan []byte, write_packet func([]byte, bool), config Config, encoders []reedsolomon.Encoder) {

	var (
		shards   [][]byte = make([][]byte, 0)
		id       uint32   = rand.Uint32()
		is_final bool
		ctx      context.Context = context.Background()
		cancel   context.CancelFunc
	)

	for {
		select {
		case b := <-enc:
			shards = append(shards, b)
			b = encode_packet(shards, id, len(shards)-1)

			write_packet(b, false)

			is_final = len(shards) == len(config.Fec)
		case <-ctx.Done():
			cancel()
			ctx = context.Background()

			is_final = true
		}

		if is_final {
			parity := final(shards, id, config.Fec[len(shards)-1], encoders[len(shards)-1])

			for i := range parity {
				write_packet(parity[i], false)
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

func decode(dec chan []byte, write_packet func([]byte, bool), config Config, encoders []reedsolomon.Encoder) {
	groups := make(map[uint32]*dec_group)

	// GC
	go func() {
		for {
			now := time.Now()
			u := time.Time{}

			for i := range groups {
				if groups[i].last_active != u {
					if groups[i].last_active.Add(time.Duration(config.GroupLive) * time.Millisecond).Before(now) {
						//fmt.Printf("GC: group %x cleared\n", i)
						delete(groups, i)
					}
				}
			}

			time.Sleep(time.Second)
		}
	}()

	for {
		b := <-dec

		id, err := read_id(b)
		if err != nil {
			fmt.Printf("%s\n", err)
			continue
		}

		// fmt.Printf("id: %x\n", id)

		if _, ok := groups[id]; !ok {
			groups[id] = &dec_group{}
		}
		group := groups[id]

		if group.finished {
			continue
		}

		b, err = group.decode_packet(b, config.Fec)
		if err != nil {
			fmt.Printf("%s\n", err)
			continue
		}

		group.last_active = time.Now()

		if b != nil {
			write_packet(b, true)
			continue
		}

		if group.content_n != 0 {
			if group.content_m == group.content_n {
				fmt.Printf("finished group %x\n", id)
				group.finished = true
				continue
			}

			if data, err := group.reconstruct(config.Fec[group.content_n-1], encoders[group.content_n-1]); err == nil {
				fmt.Printf("reconstructed group %x\n", id)
				for i := range data {
					if i >= len(group.sent) || !group.sent[i] {
						write_packet(data[i], true)
					}
				}
				group.finished = true
			} else {
				fmt.Printf("%s\n", err)
			}
		}
	}
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

	listen, err := net.ListenUDP("udp", udpAddr(config.Listen))

	if err != nil {
		panic(err)
	}

	encoders := make([]reedsolomon.Encoder, len(config.Fec))
	for i := range encoders {
		encoders[i], err = reedsolomon.New(i+1, int(config.Fec[i]))
		if err != nil {
			panic(err)
		}
	}

	var (
		from netip.AddrPort
		prev chan []byte
		next chan []byte
	)

	enc := make(chan []byte)
	dec := make(chan []byte)

	prev, next = enc, dec
	if config.IsServer {
		prev, next = dec, enc
	}

	forward, err := net.DialUDP("udp", nil, udpAddr(config.Forward))
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			buf := make([]byte, BufSize)

			n, _, err := forward.ReadFromUDPAddrPort(buf)

			if err != nil {
				fmt.Printf("%s\n", err)
				continue
			}

			next <- buf[:n]
		}
	}()

	write_packet := func(b []byte, isDec bool) {
		go func() {
			if config.IsServer == isDec {
				_, err := forward.Write(b)
				if err != nil {
					fmt.Printf("%s\n", err)
				}
			} else {
				_, err := listen.WriteToUDPAddrPort(b, from)
				if err != nil {
					fmt.Printf("%s\n", err)
				}
			}
		}()
	}

	go encode(enc, write_packet, config, encoders)
	go decode(dec, write_packet, config, encoders)

	var (
		n int
	)

	for {
		b := make([]byte, BufSize)
		n, from, err = listen.ReadFromUDPAddrPort(b)

		if err != nil {
			panic(err)
		}

		prev <- b[:n]
	}
}

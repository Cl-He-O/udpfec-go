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

	udpfecgo "github.com/Cl-He-O/udpfec-go/v2"
	"github.com/klauspost/reedsolomon"
)

type Config struct {
	Listen    string `json:"listen"`
	Forward   string `json:"forward"`
	IsServer  bool   `json:"is_server"`
	Timeout   int    `json:"timeout"`
	GroupLive int    `json:"group_live"`
	Fec       []int  `json:"fec"`
}

func udpAddr(addr string) *net.UDPAddr {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)

	if err != nil {
		panic(err)
	}
	return udpaddr
}

func Enc(enc chan []byte, writePacket func([]byte, bool), config Config, encoders []reedsolomon.Encoder) {
	group := udpfecgo.PacketGroup{Id: rand.Uint32()}

	var (
		is_final bool
		ctx      context.Context = context.Background()
		cancel   context.CancelFunc
	)

	for {
		select {
		case b := <-enc:
			group.Shards = append(group.Shards, b)
			b = group.EncodePacket(len(group.Shards) - 1)

			writePacket(b, false)

			is_final = len(group.Shards) == len(config.Fec)
		case <-ctx.Done():
			cancel()
			ctx = context.Background()

			is_final = true
		}

		if len(group.Shards) == 0 {
			continue
		}

		if is_final {
			group.Parity_n = uint16(config.Fec[len(group.Shards)-1])
			parity := group.Final(encoders[len(group.Shards)-1])

			for i := range parity {
				writePacket(parity[i], false)
			}

			group = udpfecgo.PacketGroup{Id: rand.Uint32()}

			cancel()
			ctx = context.Background()
		} else {
			ctx, cancel = context.WithTimeout(context.Background(), time.Duration(config.Timeout)*time.Millisecond)
		}
	}
}

func Dec(dec chan []byte, writePacket func([]byte, bool), config Config, encoders []reedsolomon.Encoder) {
	groups := make(map[uint32]*udpfecgo.PacketGroup)

	// GC
	go func() {
		for {
			now := time.Now()
			u := time.Time{}

			for i := range groups {
				if groups[i].LastActive != u {
					if groups[i].LastActive.Add(time.Duration(config.GroupLive) * time.Millisecond).Before(now) {
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

		id, err := udpfecgo.ReadID(b)
		if err != nil {
			fmt.Printf("%s\n", err)
			continue
		}

		// fmt.Printf("id: %x\n", id)

		if _, ok := groups[id]; !ok {
			groups[id] = &udpfecgo.PacketGroup{}
		}
		group := groups[id]

		if group.Finished {
			continue
		}

		b, err = group.DecodePacket(b, config.Fec, 256)
		if err != nil {
			fmt.Printf("%s\n", err)
			continue
		}

		group.LastActive = time.Now()

		if b != nil {
			writePacket(b, true)
			continue
		}

		if group.Content_n != 0 {
			if group.Content_m == group.Content_n {
				fmt.Printf("finished group %x\n", id)
				group.Finished = true
				continue
			}

			group.Parity_n = uint16(config.Fec[group.Content_n-1])
			if data, err := group.Reconstruct(encoders[group.Content_n-1]); err == nil {
				fmt.Printf("reconstructed group %x\n", id)
				for i := range data {
					if i >= len(group.Sent) || !group.Sent[i] {
						writePacket(data[i], true)
					}
				}
				group.Finished = true
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
		encoders[i], err = reedsolomon.New(i+1, config.Fec[i])
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
			buf := make([]byte, udpfecgo.BufSize)

			n, _, err := forward.ReadFromUDPAddrPort(buf)

			if err != nil {
				fmt.Printf("%s\n", err)
				next = nil
				return
			}

			next <- buf[:n]
		}
	}()

	writePacket := func(b []byte, isDec bool) {
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

	go Enc(enc, writePacket, config, encoders)
	go Dec(dec, writePacket, config, encoders)

	var (
		n int
	)

	for {
		b := make([]byte, udpfecgo.BufSize)
		n, from, err = listen.ReadFromUDPAddrPort(b)

		if err != nil {
			panic(err)
		}

		prev <- b[:n]
	}
}

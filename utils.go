package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/reedsolomon"
)

const (
	buf_size int = 9000
)

func extend[T any](b []T, l int) []T {
	return append(b, make([]T, l-len(b))...)
}

func pad(data [][]byte, target int) int {
	if target == 0 {
		for i := range data {
			if len(data[i]) > target {
				target = len(data[i])
			}
		}

		target += 2
	}

	for i := range data {
		if data[i] != nil {
			l := uint16(len(data[i]))
			data[i] = extend(data[i], target)
			binary.BigEndian.PutUint16(data[i][target-2:], l)
		}
	}

	return target
}

func encode_packet(shards [][]byte, id uint32, i int) []byte {
	var header []byte

	header = binary.BigEndian.AppendUint32(header, id)
	header = binary.BigEndian.AppendUint16(header, uint16(i))

	//fmt.Printf("i: %d, data: %x\n", i, s.Shards[i])

	return append(header, shards[i]...)
}

func final(shards [][]byte, id uint32, parity_n uint16, encoder reedsolomon.Encoder) [][]byte {
	content_n := len(shards)
	parity_len := pad(shards, 0)

	shards = append(shards, make([][]byte, parity_n)...)
	for i := content_n; i < len(shards); i++ {
		shards[i] = make([]byte, parity_len)
	}

	err := encoder.Encode(shards)
	if err != nil {
		panic(err)
	}

	for i := content_n; i < len(shards); i++ {
		header := binary.BigEndian.AppendUint32([]byte{}, id)
		header = binary.BigEndian.AppendUint16(header, uint16(i)|0x8000)
		header = binary.BigEndian.AppendUint16(header, uint16(content_n))

		shards[i] = append(header, shards[i]...)
	}

	return shards[content_n:]
}

type DecGroup struct {
	content_n uint16
	content_m uint16
	parity_m  uint16
	parity_l  uint16

	t        time.Time
	finished bool

	shards [][]byte
	sent   []bool
}

func (s *DecGroup) decode_packet(b []byte, fec []uint16) ([]byte, error) {
	if len(b) < 6 {
		return nil, fmt.Errorf("packet too small: %d < 6", len(b))
	}

	var (
		i         uint16
		content_n uint16
		is_parity bool
	)
	r := bytes.NewReader(b[4:])

	binary.Read(r, binary.BigEndian, &i)

	if i&0x8000 != 0 {
		if len(b) < 8 {
			return nil, fmt.Errorf("packet too small: %d < 8", len(b))
		}

		i = i & 0x7FFF
		is_parity = true

		binary.Read(r, binary.BigEndian, &content_n)
	}

	if is_parity {
		if s.content_n != 0 && content_n != s.content_n {
			return nil, fmt.Errorf("mismatch content shard number: %d != %d", content_n, s.content_n)
		}

		if content_n > uint16(len(fec)) {
			return nil, fmt.Errorf("too many content shards: %d >= %d", content_n, len(fec))
		}

		s.content_n = content_n
	}

	if s.content_n != 0 {
		if i >= s.content_n+fec[s.content_n-1] {
			return nil, fmt.Errorf("packet index out of bound: %d >= %d", i, s.content_n+uint16(fec[s.content_n-1]))
		}
	} else if i >= uint16(len(fec))+fec[len(fec)-1] { // assume fec params is incremental
		return nil, fmt.Errorf("packet index out of bound: %d >= %d", i, uint16(len(fec))+fec[s.content_n-1])
	}

	if int(i)+1 > len(s.shards) {
		s.shards = extend(s.shards, int(i)+1)
	}
	s.shards[i] = make([]byte, r.Len())
	r.Read(s.shards[i])

	//fmt.Printf("i: %d\n", i)

	if !is_parity {
		s.content_m += 1

		if int(i)+1 > len(s.sent) {
			s.sent = extend(s.sent, int(i)+1)
		}

		return s.shards[i], nil
	} else {
		if s.parity_l != 0 && s.parity_l != uint16(len(s.shards[i])) {
			return nil, fmt.Errorf("mismatch parity length: %d != %d", s.parity_l, uint16(len(s.shards[i])))
		}

		s.parity_l = uint16(len(s.shards[i]))
		s.parity_m += 1

		return nil, nil
	}
}

func (s *DecGroup) reconstruct(parity_n uint16, encoder reedsolomon.Encoder) ([][]byte, error) {
	if s.content_m+s.parity_m < s.content_n {
		return nil, nil
	}

	for i := range s.shards[:s.content_n] {
		if uint16(len(s.shards[i])) > s.parity_l {
			return nil, fmt.Errorf("content length greater than parity length: %d > %d", uint16(len(s.shards[i])), s.parity_l)
		}
	}

	s.shards = extend(s.shards, int(parity_n+s.content_n))
	pad(s.shards[:s.content_n], int(s.parity_l))

	err := encoder.ReconstructData(s.shards)

	if err != nil {
		return nil, err
	}

	var (
		r io.Reader
		l uint16
	)

	for i := range s.shards[:s.content_n] {
		r = bytes.NewReader(s.shards[i][len(s.shards[i])-2:])

		binary.Read(r, binary.BigEndian, &l)

		if l > uint16(len(s.shards[i])) {
			return nil, fmt.Errorf("content indicated length greater than actual :%d > %d", l, uint16(len(s.shards[i])))
		}
		s.shards[i] = s.shards[i][:l]
	}

	return s.shards[:s.content_n], nil
}

func read_id(b []byte) (uint32, error) {
	if len(b) < 4 {
		return 0, fmt.Errorf("packet too small: %d < 4", len(b))
	}
	r := bytes.NewReader(b)
	var id uint32
	err := binary.Read(r, binary.BigEndian, &id)

	if err != nil {
		return 0, err
	} else {
		return id, nil
	}
}

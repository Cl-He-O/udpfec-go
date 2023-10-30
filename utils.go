package udpfecgo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/reedsolomon"
)

const (
	BufSize int = 9000
)

type PacketGroup struct {
	Id        uint32
	Content_n uint16
	Content_m uint16
	Parity_n  uint16 // config
	Parity_m  uint16
	Parity_l  uint16

	LastActive time.Time
	Finished   bool

	Shards [][]byte
	Sent   []bool
}

func (s *PacketGroup) EncodePacket(i int) []byte {
	var header []byte

	header = binary.BigEndian.AppendUint32(header, s.Id)
	header = binary.BigEndian.AppendUint16(header, uint16(i))

	//fmt.Printf("i: %d, data: %x\n", i, s.Shards[i])

	return append(header, s.Shards[i]...)
}

func pad(data [][]byte, target uint16) uint16 {
	if target == 0 {
		for i := range data {
			if uint16(len(data[i])) > target {
				target = uint16(len(data[i]))
			}
		}

		target += 2
	}

	for i := range data {
		if data[i] != nil {
			l := uint16(len(data[i]))
			data[i] = append(data[i], make([]byte, target-uint16(len(data[i])))...)
			binary.BigEndian.PutUint16(data[i][target-2:], l)
		}
	}

	return target
}

func (s *PacketGroup) Final(encoder reedsolomon.Encoder) [][]byte {
	content_n := len(s.Shards)
	parity_len := pad(s.Shards, 0)

	s.Shards = append(s.Shards, make([][]byte, s.Parity_n)...)
	for i := content_n; i < len(s.Shards); i++ {
		s.Shards[i] = make([]byte, parity_len)
	}

	err := encoder.Encode(s.Shards)
	if err != nil {
		panic(err)
	}

	for i := content_n; i < len(s.Shards); i++ {
		header := binary.BigEndian.AppendUint32([]byte{}, s.Id)
		header = binary.BigEndian.AppendUint16(header, uint16(i)|0x8000)
		header = binary.BigEndian.AppendUint16(header, uint16(content_n))

		s.Shards[i] = append(header, s.Shards[i]...)
	}

	return s.Shards[content_n:]
}

func (s *PacketGroup) DecodePacket(b []byte, fec []int, max_total_n uint16) ([]byte, error) {
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
		if s.Content_n != 0 && content_n != s.Content_n {
			return nil, fmt.Errorf("mismatch content shard number: %d != %d", content_n, s.Content_n)
		}

		if content_n > max_total_n {
			return nil, fmt.Errorf("too many content shards: %d >= %d", content_n, max_total_n)
		}

		s.Content_n = content_n
	}

	if s.Content_n != 0 {
		if i >= s.Content_n+uint16(fec[s.Content_n-1]) {
			return nil, fmt.Errorf("packet index out of bound: %d >= %d", i, s.Content_n+uint16(fec[s.Content_n-1]))
		}
	} else if i >= max_total_n {
		return nil, fmt.Errorf("packet index out of bound: %d >= %d", i, max_total_n)
	}

	if int(i)+1 > len(s.Shards) {
		s.Shards = append(s.Shards, make([][]byte, int(i)+1-len(s.Shards))...)
	}
	s.Shards[i] = make([]byte, r.Len())
	r.Read(s.Shards[i])

	//fmt.Printf("i: %d\n", i)

	if !is_parity {
		s.Content_m += 1
		if int(i)+1 > len(s.Sent) {
			s.Sent = append(s.Sent, make([]bool, int(i)+1-len(s.Sent))...)
		}
		return s.Shards[i], nil
	} else {
		s.Parity_m += 1
		s.Parity_l = uint16(len(s.Shards[i]))
		return nil, nil
	}
}

func (s *PacketGroup) Reconstruct(encoder reedsolomon.Encoder) ([][]byte, error) {
	if s.Content_m+s.Parity_m < s.Content_n {
		return nil, fmt.Errorf("")
	}

	s.Shards = append(s.Shards, make([][]byte, s.Parity_n+s.Content_n-uint16(len(s.Shards)))...)

	pad(s.Shards[:s.Content_n], s.Parity_l)

	err := encoder.ReconstructData(s.Shards)

	if err != nil {
		return nil, err
	}

	var (
		r io.Reader
		l uint16
	)

	for i := range s.Shards[:s.Content_n] {
		r = bytes.NewReader(s.Shards[i][len(s.Shards[i])-2:])

		binary.Read(r, binary.BigEndian, &l)
		s.Shards[i] = s.Shards[i][:l]

		//fmt.Printf("i: %d, data: %x\n", i, s.Shards[i])
	}

	return s.Shards[:s.Content_n], nil
}

func ReadID(b []byte) (uint32, error) {
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

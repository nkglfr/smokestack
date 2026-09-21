package main

import (
	"encoding/binary"
	"math"
	"sort"
)

// Sketch est un histogramme a buckets logarithmiques (DDSketch).
// gamma = 1.02 garantit une erreur relative de 1% sur tous les
// percentiles, et l'addition bucket a bucket rend la fusion exacte :
// le p95 d'une heure calcule depuis 60 sketches d'une minute est
// identique au p95 calcule sur les echantillons bruts, a 1% pres.
const sketchGamma = 1.02

var logGamma = math.Log(sketchGamma)

type Sketch struct {
	buckets map[int32]uint32
	count   uint64
	min     float64
	max     float64
}

func NewSketch() *Sketch {
	return &Sketch{buckets: make(map[int32]uint32, 48), min: math.Inf(1)}
}

func sketchIndex(us float64) int32 {
	if us < 1 {
		us = 1
	}
	return int32(math.Ceil(math.Log(us) / logGamma))
}

func sketchValue(i int32) float64 {
	return math.Pow(sketchGamma, float64(i)) * 2 / (1 + sketchGamma)
}

// Add insere un echantillon exprime en microsecondes.
func (s *Sketch) Add(us float64) {
	s.buckets[sketchIndex(us)]++
	s.count++
	if us < s.min {
		s.min = us
	}
	if us > s.max {
		s.max = us
	}
}

func (s *Sketch) addRaw(idx int32, n uint32) {
	if n == 0 {
		return
	}
	s.buckets[idx] += n
	s.count += uint64(n)
	v := sketchValue(idx)
	if v < s.min {
		s.min = v
	}
	if v > s.max {
		s.max = v
	}
}

// Merge additionne un autre sketch dans celui-ci.
func (s *Sketch) Merge(o *Sketch) {
	if o == nil {
		return
	}
	for idx, n := range o.buckets {
		s.buckets[idx] += n
		s.count += uint64(n)
	}
	if o.min < s.min {
		s.min = o.min
	}
	if o.max > s.max {
		s.max = o.max
	}
}

func (s *Sketch) Count() uint64 { return s.count }

func (s *Sketch) Min() float64 {
	if s.count == 0 {
		return 0
	}
	return s.min
}

func (s *Sketch) Max() float64 {
	if s.count == 0 {
		return 0
	}
	return s.max
}

// Quantile retourne le percentile demande (0..1) en microsecondes.
func (s *Sketch) Quantile(q float64) float64 {
	if s.count == 0 {
		return 0
	}
	if q <= 0 {
		return s.min
	}
	if q >= 1 {
		return s.max
	}
	idxs := s.sortedIndexes()
	rank := q * float64(s.count-1)
	var seen float64
	for _, idx := range idxs {
		seen += float64(s.buckets[idx])
		if seen > rank {
			v := sketchValue(idx)
			if v < s.min {
				v = s.min
			}
			if v > s.max {
				v = s.max
			}
			return v
		}
	}
	return s.max
}

func (s *Sketch) sortedIndexes() []int32 {
	idxs := make([]int32, 0, len(s.buckets))
	for idx := range s.buckets {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })
	return idxs
}

// MarshalBinary serialise le sketch : nombre d'entrees, puis pour
// chaque bucket le delta d'index en zigzag varint et le compteur en
// varint. Un sketch typique de 20 echantillons tient en 60 a 150 octets.
func (s *Sketch) MarshalBinary() []byte {
	idxs := s.sortedIndexes()
	buf := make([]byte, 0, 16+len(idxs)*3)
	var tmp [binary.MaxVarintLen64]byte

	n := binary.PutUvarint(tmp[:], uint64(len(idxs)))
	buf = append(buf, tmp[:n]...)

	var prev int32
	for _, idx := range idxs {
		n = binary.PutVarint(tmp[:], int64(idx-prev))
		buf = append(buf, tmp[:n]...)
		n = binary.PutUvarint(tmp[:], uint64(s.buckets[idx]))
		buf = append(buf, tmp[:n]...)
		prev = idx
	}

	n = binary.PutUvarint(tmp[:], uint64(math.Float32bits(float32(s.Min()))))
	buf = append(buf, tmp[:n]...)
	n = binary.PutUvarint(tmp[:], uint64(math.Float32bits(float32(s.Max()))))
	buf = append(buf, tmp[:n]...)
	return buf
}

func UnmarshalSketch(b []byte) *Sketch {
	s := NewSketch()
	if len(b) == 0 {
		return s
	}
	entries, n := binary.Uvarint(b)
	if n <= 0 {
		return s
	}
	pos := n
	var prev int32
	for i := uint64(0); i < entries; i++ {
		d, n := binary.Varint(b[pos:])
		if n <= 0 {
			return s
		}
		pos += n
		c, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return s
		}
		pos += n
		idx := prev + int32(d)
		s.buckets[idx] += uint32(c)
		s.count += c
		prev = idx
	}
	if mn, n := binary.Uvarint(b[pos:]); n > 0 {
		pos += n
		s.min = float64(math.Float32frombits(uint32(mn)))
		if mx, n := binary.Uvarint(b[pos:]); n > 0 {
			s.max = float64(math.Float32frombits(uint32(mx)))
		}
	}
	if s.count == 0 {
		s.min = math.Inf(1)
		s.max = 0
	}
	return s
}

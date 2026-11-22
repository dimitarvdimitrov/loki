package dataset

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/axiomhq/fsst"

	"github.com/grafana/loki/v3/pkg/columnar"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/datasetmd"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/streamio"
	"github.com/grafana/loki/v3/pkg/memory"
)

func init() {
	registerValueEncoding(datasetmd.PHYSICAL_TYPE_BINARY, datasetmd.ENCODING_TYPE_FSST, registryEntry{
		NewEncoder: func(w streamio.Writer) valueEncoder { return &fsstEncoder{w: w} },
		NewDecoder: func(data []byte) valueDecoder { return &fsstDecoder{data: data} },
	})
}

// fsstEncoder owns the input strings until the page trains its table at flush.
type fsstEncoder struct {
	w           streamio.Writer
	inputs      [][]byte
	decodedSize int
}

func (*fsstEncoder) PhysicalType() datasetmd.PhysicalType { return datasetmd.PHYSICAL_TYPE_BINARY }
func (*fsstEncoder) EncodingType() datasetmd.EncodingType { return datasetmd.ENCODING_TYPE_FSST }

func (enc *fsstEncoder) Encode(v Value) error {
	if v.Type() != datasetmd.PHYSICAL_TYPE_BINARY {
		return fmt.Errorf("fsst: invalid value type %v", v.Type())
	}
	enc.inputs = append(enc.inputs, bytes.Clone(v.Binary()))
	enc.decodedSize += len(v.Binary())
	return nil
}

// EstimatedSize includes the directory, worst-case table, and buffered input bytes.
func (enc *fsstEncoder) EstimatedSize() int {
	if len(enc.inputs) == 0 {
		return 0
	}
	// TODO dimitarvdimitrov too many magic numbers
	return 16 + 255*8 + 4*(2*len(enc.inputs)+1) + enc.decodedSize
}

func (enc *fsstEncoder) Flush() error {
	// TODO dimitarvdimitrov this is too ugly
	table := fsst.Train(enc.inputs)
	symbols, offsets := table.EncodeBatch(nil, nil, enc.inputs)
	if uint64(len(symbols)) > math.MaxUint32 {
		return fmt.Errorf("fsst: encoded page exceeds uint32 offsets")
	}
	if _, err := table.WriteTo(enc.w); err != nil {
		return err
	}
	var word [4]byte
	write := func(v int) error {
		if uint64(v) > math.MaxUint32 {
			return fmt.Errorf("fsst: value exceeds uint32 length")
		}
		binary.LittleEndian.PutUint32(word[:], uint32(v))
		n, err := enc.w.Write(word[:])
		if err != nil {
			return err
		}
		if n != len(word) {
			return io.ErrShortWrite
		}
		return nil
	}
	for _, off := range offsets {
		if err := write(off); err != nil {
			return err
		}
	}
	for _, input := range enc.inputs {
		if err := write(len(input)); err != nil {
			return err
		}
	}
	n, err := enc.w.Write(symbols)
	if err != nil {
		return err
	}
	if n != len(symbols) {
		return io.ErrShortWrite
	}
	return nil
}

func (enc *fsstEncoder) Reset(w streamio.Writer) {
	enc.w = w
	clear(enc.inputs)
	enc.inputs = enc.inputs[:0]
	enc.decodedSize = 0
}

// fsstDecoder retains the encoded page and decodes only the requested dense batch.
// count comes from PageDesc.ValuesCount, after construction or Reset.
// TODO dimitarvdimitrov wtf
type fsstDecoder struct {
	data       []byte
	count      int
	row        int
	table      *fsst.Table
	offsets    []byte
	lengths    []byte
	symbols    []byte
	srcOffsets []int
	dstOffsets []int
}

func (*fsstDecoder) PhysicalType() datasetmd.PhysicalType { return datasetmd.PHYSICAL_TYPE_BINARY }
func (*fsstDecoder) EncodingType() datasetmd.EncodingType { return datasetmd.ENCODING_TYPE_FSST }

func (dec *fsstDecoder) init() error {
	table := new(fsst.Table)
	r := bytes.NewReader(dec.data)
	if _, err := table.ReadFrom(r); err != nil {
		return fmt.Errorf("fsst: read table: %w", err)
	}
	rest := dec.data[len(dec.data)-r.Len():]
	if dec.count < 0 || uint64(dec.count)*8+4 > uint64(len(rest)) {
		return fmt.Errorf("fsst: truncated directory")
	}
	dec.offsets = rest[:4*(dec.count+1)]
	dec.lengths = rest[len(dec.offsets) : len(dec.offsets)+4*dec.count]
	dec.symbols = rest[len(dec.offsets)+len(dec.lengths):]
	previous := uint32(0)
	for i := 0; i <= dec.count; i++ {
		off := binary.LittleEndian.Uint32(dec.offsets[4*i:])
		if (i == 0 && off != 0) || off < previous || uint64(off) > uint64(len(dec.symbols)) {
			return fmt.Errorf("fsst: invalid symbol offset")
		}
		previous = off
	}
	if uint64(previous) != uint64(len(dec.symbols)) {
		return fmt.Errorf("fsst: invalid final symbol offset")
	}
	dec.table = table
	return nil
}

func (dec *fsstDecoder) Decode(alloc *memory.Allocator, count int) (columnar.Array, error) {
	if dec.table == nil {
		if err := dec.init(); err != nil {
			return nil, err
		}
	}
	if dec.row == dec.count {
		return nil, io.EOF
	}
	n := min(count, dec.count-dec.row)
	if n < 0 {
		return nil, fmt.Errorf("fsst: negative batch size")
	}
	base := int(binary.LittleEndian.Uint32(dec.offsets[4*dec.row:]))
	dec.srcOffsets = dec.srcOffsets[:0]
	total := uint64(0)
	for i := 0; i <= n; i++ {
		dec.srcOffsets = append(dec.srcOffsets, int(binary.LittleEndian.Uint32(dec.offsets[4*(dec.row+i):]))-base)
		if i < n {
			total += uint64(binary.LittleEndian.Uint32(dec.lengths[4*(dec.row+i):]))
		}
	}
	if total > math.MaxInt32 {
		return nil, fmt.Errorf("fsst: decoded batch exceeds int32 offsets")
	}
	valuesBuf := memory.NewBuffer[byte](alloc, int(total))
	offsetsBuf := memory.NewBuffer[int32](alloc, n+1)
	values := valuesBuf.Data()[:int(total)]
	offsets := offsetsBuf.Data()[:n+1]
	decoded, decodedOffsets, err := dec.table.DecodeBatch(values[:0], dec.dstOffsets[:0],
		dec.symbols[base:base+dec.srcOffsets[n]], dec.srcOffsets)
	if err != nil {
		return nil, fmt.Errorf("fsst: decode batch: %w", err)
	}
	dec.dstOffsets = decodedOffsets
	if len(decoded) != int(total) {
		return nil, fmt.Errorf("fsst: decoded size mismatch")
	}
	for i := range n {
		if decodedOffsets[i+1]-decodedOffsets[i] != int(binary.LittleEndian.Uint32(dec.lengths[4*(dec.row+i):])) {
			return nil, fmt.Errorf("fsst: decoded string length mismatch")
		}
		offsets[i] = int32(decodedOffsets[i])
	}
	offsets[n] = int32(total)
	// DecodeBatch can grow its output for corrupt lengths. Copy into allocator-owned memory.
	copy(values, decoded)
	dec.row += n
	array := columnar.NewUTF8(values, offsets, memory.Bitmap{})
	if n < count {
		return array, io.EOF
	}
	return array, nil
}

func (dec *fsstDecoder) Reset(data []byte) {
	dec.data = data
	dec.count, dec.row = 0, 0
	dec.table = nil
	dec.offsets, dec.lengths, dec.symbols = nil, nil, nil
}

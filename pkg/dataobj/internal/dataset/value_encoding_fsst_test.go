package dataset

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/v3/pkg/columnar"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/datasetmd"
	"github.com/grafana/loki/v3/pkg/memory"
)

func TestFSSTPage(t *testing.T) {
	for _, inputs := range [][]Value{
		{BinaryValue([]byte("hello")), BinaryValue(nil), Value{}, BinaryValue([]byte("world"))},
		{Value{}, Value{}},
		{BinaryValue(nil), BinaryValue(nil)},
		{BinaryValue([]byte{0, 255, 1}), BinaryValue([]byte("hello 世界"))},
	} {
		opts := BuilderOptions{
			Type:        ColumnType{Physical: datasetmd.PHYSICAL_TYPE_BINARY},
			Encoding:    datasetmd.ENCODING_TYPE_FSST,
			Compression: datasetmd.COMPRESSION_TYPE_NONE,
		}
		b, err := newPageBuilder(opts)
		require.NoError(t, err)
		var pr *pageReader
		for range 2 { // Reuse the builder and decoder state across page boundaries.
			decodedSize := 0
			for _, v := range inputs {
				require.True(t, b.Append(v))
				if !v.IsNil() {
					decodedSize += len(v.Binary())
				}
			}
			page, err := b.Flush()
			require.NoError(t, err)
			bitmapSize, headerSize := binary.Uvarint(page.Data)
			require.Equal(t, headerSize+int(bitmapSize)+decodedSize, page.Desc.UncompressedSize)
			require.Equal(t, len(page.Data), page.Desc.CompressedSize)
			var alloc memory.Allocator
			if pr == nil {
				pr = newPageReader(page, opts.Type.Physical, opts.Compression)
			} else {
				pr.Reset(page, opts.Type.Physical, opts.Compression)
			}
			for range 2 {
				for i := range inputs {
					arr, err := pr.Read(context.Background(), &alloc, 1)
					require.NoError(t, err)
					if inputs[i].IsNil() {
						require.True(t, arr.(interface{ IsNull(int) bool }).IsNull(0))
					} else {
						require.Equal(t, string(inputs[i].Binary()), string(arr.(*columnar.UTF8).Get(0)))
					}
				}
				_, err := pr.Seek(0, io.SeekStart)
				require.NoError(t, err)
			}
			require.NoError(t, pr.Close())
			alloc.Free()
		}
	}
}

func TestFSSTOwnsInputAndDecodesBatches(t *testing.T) {
	var buf bytes.Buffer
	enc := &fsstEncoder{w: &buf}
	input := []byte("hello world")
	require.NoError(t, enc.Encode(BinaryValue(input)))
	input[0] = 'X'
	require.NoError(t, enc.Encode(BinaryValue(nil)))
	require.NoError(t, enc.Encode(BinaryValue([]byte("another log message"))))
	require.Positive(t, enc.EstimatedSize())
	require.NoError(t, enc.Flush())

	var alloc memory.Allocator
	defer alloc.Free()
	dec := &fsstDecoder{data: buf.Bytes(), count: 3}
	arr, err := dec.Decode(&alloc, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"hello world", ""}, convertToStrings(t, arr))
	arr, err = dec.Decode(&alloc, 2)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, []string{"another log message"}, convertToStrings(t, arr))
	_, err = dec.Decode(&alloc, 1)
	require.ErrorIs(t, err, io.EOF)

	dec.Reset(buf.Bytes())
	dec.count = 3
	arr, err = dec.Decode(&alloc, 3)
	require.NoError(t, err)
	require.Equal(t, []string{"hello world", "", "another log message"}, convertToStrings(t, arr))
	dec.Reset(buf.Bytes())
	dec.count, dec.row = 3, 2
	arr, err = dec.Decode(&alloc, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"another log message"}, convertToStrings(t, arr))
}

func TestFSSTRejectsCorruptDirectory(t *testing.T) {
	var buf bytes.Buffer
	enc := &fsstEncoder{w: &buf}
	require.NoError(t, enc.Encode(BinaryValue([]byte("hello world"))))
	require.NoError(t, enc.Flush())
	dec := &fsstDecoder{data: buf.Bytes(), count: 1}
	require.NoError(t, dec.init())
	offsetPos := len(dec.data) - len(dec.symbols) - len(dec.lengths) - len(dec.offsets)
	lengthPos := offsetPos + len(dec.offsets)
	for _, pos := range []int{offsetPos, offsetPos + 4, lengthPos} {
		data := bytes.Clone(buf.Bytes())
		binary.LittleEndian.PutUint32(data[pos:], 999)
		bad := &fsstDecoder{data: data, count: 1}
		var alloc memory.Allocator
		_, err := bad.Decode(&alloc, 1)
		require.Error(t, err)
		alloc.Free()
	}
	for _, size := range []int{0, 8, lengthPos + 2} {
		bad := &fsstDecoder{data: buf.Bytes()[:size], count: 1}
		var alloc memory.Allocator
		_, err := bad.Decode(&alloc, 1)
		require.Error(t, err)
		alloc.Free()
	}
}

func TestFSSTSkip(t *testing.T) {
	var buf bytes.Buffer
	enc := &fsstEncoder{w: &buf}
	for _, input := range []string{"first", "", "世界", "last"} {
		require.NoError(t, enc.Encode(BinaryValue([]byte(input))))
	}
	require.NoError(t, enc.Flush())
	dec := &fsstDecoder{data: buf.Bytes(), count: 4}
	for _, count := range []int{-1, 0} {
		n, err := dec.Skip(count)
		require.NoError(t, err)
		require.Zero(t, n)
	}
	n, err := dec.Skip(2)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	var alloc memory.Allocator
	defer alloc.Free()
	arr, err := dec.Decode(&alloc, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"世界"}, convertToStrings(t, arr))
	n, err = dec.Skip(100)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = dec.Skip(1)
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = dec.Decode(&alloc, 1)
	require.ErrorIs(t, err, io.EOF)
	dec.Reset(buf.Bytes())
	dec.count = 4
	arr, err = dec.Decode(&alloc, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"first"}, convertToStrings(t, arr))
	bad := &fsstDecoder{data: []byte{1}, count: 4}
	_, err = bad.Skip(1)
	require.Error(t, err)
}

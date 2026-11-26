package objtest

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"

	"github.com/grafana/loki/v3/pkg/dataobj"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/datasetmd"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/logs"
)

// RequireMessageEncoding checks every message page and its column compression.
// Optional tenants restrict the check to those tenants.
// It returns the page count and fails if the selected sections contain no message pages.
func RequireMessageEncoding(t testing.TB, obj *dataobj.Object, encoding logs.EncodingType, tenants ...string) int {
	t.Helper()
	require.Contains(t, []logs.EncodingType{logs.EncodingTypePlain, logs.EncodingTypeFSST}, encoding)
	compression := datasetmd.COMPRESSION_TYPE_ZSTD
	if encoding == logs.EncodingTypeFSST {
		compression = datasetmd.COMPRESSION_TYPE_NONE
	}
	pages := 0
	for _, sec := range obj.Sections().Filter(logs.CheckSection) {
		if len(tenants) > 0 && !slices.Contains(tenants, sec.Tenant) {
			continue
		}
		section, err := logs.Open(t.Context(), sec)
		require.NoError(t, err)
		dset, err := logs.MakeColumnarDataset(section)
		require.NoError(t, err)
		for i, column := range dset.Columns() {
			if section.Columns()[i].Type != logs.ColumnTypeMessage {
				continue
			}
			require.Equal(t, compression, column.ColumnDesc().Compression)
			columnPages := 0
			for result := range column.ListPages(t.Context()) {
				page, err := result.Value()
				require.NoError(t, err)
				require.Equal(t, encoding, page.PageDesc().Encoding)
				columnPages++
			}
			require.Positive(t, columnPages)
			pages += columnPages
		}
	}
	require.Positive(t, pages, "the object must contain message pages")
	return pages
}

// RequireMessageEncoding checks every logs object in the builder's bucket after Close.
// It returns the total message page count.
func (b *Builder) RequireMessageEncoding(encoding logs.EncodingType) int {
	b.t.Helper()
	require.True(b.t, b.closed, "call Close before the encoding check")
	pages := 0
	err := b.bucket.Iter(b.t.Context(), "", func(name string) error {
		if strings.HasSuffix(name, "/") || !strings.Contains(name, "objects") {
			return nil
		}
		obj, err := dataobj.FromBucket(b.t.Context(), b.bucket, name, 0)
		if err != nil {
			return err
		}
		if obj.Sections().Count(logs.CheckSection) > 0 {
			pages += RequireMessageEncoding(b.t, obj, encoding)
		}
		return nil
	}, objstore.WithRecursiveIter())
	require.NoError(b.t, err)
	require.Positive(b.t, pages)
	return pages
}

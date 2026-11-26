package logsobj

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/push"

	"github.com/grafana/loki/v3/pkg/dataobj"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/datasetmd"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/logs"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/validation"
)

func TestBuilder_FSSTPerTenant(t *testing.T) {
	var defaults validation.Limits
	defaults.RegisterFlags(flag.NewFlagSet("test", flag.PanicOnError))
	tenantLimits := limitsByTenant{
		"enabled":  {DataObjLogColumnCompression: validation.DataObjLogColumnCompressionFSST},
		"disabled": {DataObjLogColumnCompression: validation.DataObjLogColumnCompressionZstd},
	}
	overrides, err := validation.NewOverrides(defaults, tenantLimits)
	require.NoError(t, err)
	cfg := testBuilderConfig
	cfg.MaxPageRows = 2
	cfg.TargetSectionSize = 512
	lines := []string{"", "hello 世界", string([]byte{0, 255, 1}), strings.Repeat("message ", 50), "last line"}
	for _, tenant := range []string{"enabled", "disabled", "default"} {
		for _, sortObject := range []bool{false, true} {
			t.Run(fmt.Sprintf("tenant=%s/sort=%t", tenant, sortObject), func(t *testing.T) {
				tenantLimits["enabled"].DataObjLogColumnCompression = validation.DataObjLogColumnCompressionFSST
				builder, err := NewBuilder(tenant, cfg, nil, NewBuilderMetrics(), log.NewNopLogger(), overrides)
				require.NoError(t, err)
				for round := range 2 {
					for i, line := range lines {
						require.NoError(t, builder.Append(logproto.Stream{Labels: `{app="test"}`, Entries: []push.Entry{{Timestamp: time.Unix(int64(i+1), 0), Line: line}}}, time.Unix(10, 0)))
					}
					var obj *dataobj.Object
					var closer io.Closer
					if sortObject {
						obj, closer, err = builder.FlushAndSort(t.Context())
					} else {
						obj, closer, err = builder.Flush()
					}
					require.NoError(t, err)
					requireObjectMessageEncoding(t, obj, map[string]bool{tenant: tenant == "enabled" && round == 0}, lines)
					require.NoError(t, closer.Close())
					// The next object uses the updated override after Reset.
					tenantLimits["enabled"].DataObjLogColumnCompression = validation.DataObjLogColumnCompressionZstd
					builder.Reset()
				}
			})
		}
	}
}

func TestBuilder_FSSTWithoutOverrides(t *testing.T) {
	builder, err := NewBuilder("tenant", testBuilderConfig, nil, NewBuilderMetrics(), log.NewNopLogger(), NoTenantOverrides{})
	require.NoError(t, err)
	require.NoError(t, builder.Append(logproto.Stream{Labels: `{app="test"}`, Entries: []push.Entry{{Timestamp: time.Unix(1, 0), Line: "hello"}}}, time.Unix(2, 0)))
	obj, closer, err := builder.Flush()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closer.Close()) })
	requireObjectMessageEncoding(t, obj, map[string]bool{"tenant": false}, []string{"hello"})
}

func requireObjectMessageEncoding(t *testing.T, obj *dataobj.Object, enabled map[string]bool, wantLines []string) {
	t.Helper()
	got := make(map[string][]string)
	pages := make(map[string]int)
	for _, sec := range obj.Sections().Filter(logs.CheckSection) {
		section, err := logs.Open(t.Context(), sec)
		require.NoError(t, err)
		dset, err := logs.MakeColumnarDataset(section)
		require.NoError(t, err)
		for i, column := range dset.Columns() {
			if section.Columns()[i].Type != logs.ColumnTypeMessage {
				continue
			}
			encoding, compression := datasetmd.ENCODING_TYPE_PLAIN, datasetmd.COMPRESSION_TYPE_ZSTD
			if enabled[sec.Tenant] {
				encoding, compression = datasetmd.ENCODING_TYPE_FSST, datasetmd.COMPRESSION_TYPE_NONE
			}
			require.Equal(t, compression, column.ColumnDesc().Compression)
			for res := range column.ListPages(t.Context()) {
				page, err := res.Value()
				require.NoError(t, err)
				require.Equal(t, encoding, page.PageDesc().Encoding)
				pages[sec.Tenant]++
			}
		}
		for res := range logs.IterSection(t.Context(), section) {
			record, err := res.Value()
			require.NoError(t, err)
			got[sec.Tenant] = append(got[sec.Tenant], string(record.Line))
		}
	}
	require.Len(t, got, len(enabled))
	for tenant := range enabled {
		require.Positive(t, pages[tenant])
		require.ElementsMatch(t, wantLines, got[tenant], "tenant %s", tenant)
	}
}

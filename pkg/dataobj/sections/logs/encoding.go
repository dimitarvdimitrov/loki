package logs

import "github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/datasetmd"

// EncodingType is the page encoding from the data object metadata.
type EncodingType = datasetmd.EncodingType

const (
	// EncodingTypePlain stores plain message values with ZSTD compression.
	EncodingTypePlain = datasetmd.ENCODING_TYPE_PLAIN
	// EncodingTypeFSST stores FSST message values without outer compression.
	EncodingTypeFSST = datasetmd.ENCODING_TYPE_FSST
)

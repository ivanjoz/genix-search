package pricing

import (
	"math"
	"testing"
)

func TestS3Express(t *testing.T) {
	p := S3Express

	if got := p.BytesPerGET(); got < 49_000 || got > 54_000 {
		t.Errorf("BytesPerGET = %d, want about 53 687 (0.00003/1000 over 0.0006/2^30)", got)
	}

	if got := p.BytesPerPUT(); got < 370_000 || got > 380_000 {
		t.Errorf("BytesPerPUT = %d, want about 379 165", got)
	}

	// A million cold queries of 4 GETs and 8 KB.
	if got := 1e6 * p.Read(4, 8<<10); math.Abs(got-(0.12+0.0046)) > 1e-3 {
		t.Errorf("1M queries of 4 GETs and 8 KB = $%.4f, want about $0.1246", got)
	}
}

// Package pricing is what S3 bills for the requests and bytes of the index:
// the yardstick every read and write is weighed with.
package pricing

// GB is the gigabyte AWS bills by (2^30 bytes).
const GB = 1 << 30

// Prices are dollars per request, per byte or per byte-month.
type Prices struct {
	// GET is a GET or any other request but the writes (HEAD included).
	GET float64
	// PUT is a PUT, COPY, POST or LIST request.
	PUT float64
	// Retrieval and Upload are per byte read and per byte written: every
	// byte, not only those over 512 KB per request (the rule before 2025).
	Retrieval, Upload float64
	// Storage is per byte-month.
	Storage float64
}

// S3Express is S3 Express One Zone in us-east-1, from the AWS Price List API
// (pricing.us-east-1.amazonaws.com, offer AmazonS3, published 2026-09-28):
//
//	Requests-XZ-Tier2   $0.00003 per 1,000 GET and all other requests
//	Requests-XZ-Tier1   $0.00113 per 1,000 PUT, COPY, POST or LIST requests
//	Retrieval-XZ        $0.0006 per GB retrieved
//	Upload-XZ           $0.0032 per GB uploaded
//	TimedStorage-XZ     $0.11 per GB-month
var S3Express = Prices{
	GET:       0.00003 / 1000,
	PUT:       0.00113 / 1000,
	Retrieval: 0.0006 / GB,
	Upload:    0.0032 / GB,
	Storage:   0.11 / GB,
}

// Read is the cost of gets requests that return bytes.
func (p Prices) Read(gets int, bytes int64) float64 {
	return float64(gets)*p.GET + float64(bytes)*p.Retrieval
}

// Write is the cost of puts requests that upload bytes.
func (p Prices) Write(puts int, bytes int64) float64 {
	return float64(puts)*p.PUT + float64(bytes)*p.Upload
}

// BytesPerGET is the bytes whose retrieval costs as much as one GET: reading
// fewer extra bytes to save a request pays off. 53 687 bytes (52 KiB) for S3
// Express.
func (p Prices) BytesPerGET() int64 {
	return int64(p.GET / p.Retrieval)
}

// BytesPerPUT is the bytes whose upload costs as much as one PUT: 379 165
// bytes (370 KiB) for S3 Express.
func (p Prices) BytesPerPUT() int64 {
	return int64(p.PUT / p.Upload)
}

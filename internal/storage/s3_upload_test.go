package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 records the object-storage calls uploadFile makes. It answers the
// S3 REST shapes the SDK uses for PutObject, CreateMultipartUpload,
// UploadPart and CompleteMultipartUpload with just enough XML to satisfy the
// client; the point is to observe which route a given size takes and how
// many bytes reach the bucket.
type fakeS3 struct {
	mu        sync.Mutex
	putBytes  int64
	putCalls  int
	partSizes map[int]int64
	completed int
	aborted   int
}

func (f *fakeS3) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := io.Copy(io.Discard, r.Body)
		var part int
		fmt.Sscan(q.Get("partNumber"), &part)
		if f.partSizes == nil {
			f.partSizes = map[int]int64{}
		}
		f.partSizes[part] = n
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, part))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var body struct {
			XMLName xml.Name `xml:"CompleteMultipartUpload"`
			Parts   []struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			} `xml:"Part"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(raw, &body); err != nil || len(body.Parts) != len(f.partSizes) {
			http.Error(w, "bad complete request", http.StatusBadRequest)
			return
		}
		f.completed++
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		n, _ := io.Copy(io.Discard, r.Body)
		f.putBytes += n
		f.putCalls++
		w.Header().Set("ETag", `"put"`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}
}

func newFakeS3Client(t *testing.T, f *fakeS3) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider("k", "s", ""),
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
	})
}

func tempFileOfSize(t *testing.T, size int64) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// A single PutObject is capped at 5 GB by S3 and R2. Multi-hour recordings
// exceed that, and before this the whole capture failed with EntityTooLarge
// three times over — re-downloading the video on every attempt. Anything
// above the threshold must go up in parts and reassemble to the full size.
func TestUploadFileUsesMultipartForLargeObjects(t *testing.T) {
	old := multipartThresholdForTest
	defer func() { multipartThresholdForTest = old }()
	multipartThresholdForTest = 1 << 20

	f := &fakeS3{}
	client := newFakeS3Client(t, f)
	size := int64(multipartPartSize)*2 + 12345
	file := tempFileOfSize(t, size)

	if err := uploadFile(context.Background(), client, "b", "k", file); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if f.putCalls != 0 {
		t.Errorf("large object went through PutObject %d times; want multipart", f.putCalls)
	}
	if f.completed != 1 || f.aborted != 0 {
		t.Errorf("completed=%d aborted=%d; want 1/0", f.completed, f.aborted)
	}
	var total int64
	for _, n := range f.partSizes {
		total += n
	}
	if len(f.partSizes) != 3 || total != size {
		t.Errorf("parts=%d totalling %d bytes; want 3 parts totalling %d", len(f.partSizes), total, size)
	}
	for part, n := range f.partSizes {
		if part != len(f.partSizes) && n != multipartPartSize {
			t.Errorf("part %d is %d bytes; R2 requires every non-final part to be exactly %d", part, n, multipartPartSize)
		}
	}
}

// Small objects keep the single-request path: one PutObject, no multipart
// bookkeeping.
func TestUploadFileUsesPutObjectForSmallObjects(t *testing.T) {
	f := &fakeS3{}
	client := newFakeS3Client(t, f)
	file := tempFileOfSize(t, 4096)
	if err := uploadFile(context.Background(), client, "b", "k", file); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if f.putCalls != 1 || f.putBytes != 4096 || len(f.partSizes) != 0 {
		t.Errorf("putCalls=%d putBytes=%d parts=%d; want a single 4096-byte PutObject", f.putCalls, f.putBytes, len(f.partSizes))
	}
}

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lightshipHQ/lightship/internal/traces"
)

type stubSpanReader struct {
	spans []traces.Span
	next  *traces.Cursor
	err   error
	at    int
}

func (s *stubSpanReader) Read() (traces.Span, bool, error) {
	if s.at < len(s.spans) {
		span := s.spans[s.at]
		s.at++
		return span, true, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return nil, false, err
	}
	return nil, false, nil
}

func (s *stubSpanReader) NextCursor() *traces.Cursor { return s.next }

type observingWriter struct {
	*httptest.ResponseRecorder
	wrote *bool
}

func (w observingWriter) Write(p []byte) (int, error) {
	*w.wrote = true
	return w.ResponseRecorder.Write(p)
}

type orderedSpanReader struct {
	wrote *bool
	at    int
}

func (s *orderedSpanReader) Read() (traces.Span, bool, error) {
	s.at++
	switch s.at {
	case 1:
		return traces.Span{"TraceId": "trace-1"}, true, nil
	case 2:
		if !*s.wrote {
			return nil, false, errors.New("second row read before first row was written")
		}
		return traces.Span{"TraceId": "trace-2"}, true, nil
	default:
		return nil, false, nil
	}
}

func (*orderedSpanReader) NextCursor() *traces.Cursor { return nil }

func TestWriteSpanPageStreamsValidExistingShape(t *testing.T) {
	next := &traces.Cursor{StartNanos: 42, TraceID: "trace-2"}
	rows := &stubSpanReader{
		spans: []traces.Span{{"TraceId": "trace-1"}, {"TraceId": "trace-2"}},
		next:  next,
	}
	rec := httptest.NewRecorder()
	count, started, err := writeSpanPage(rec, map[string]string{"trace_id": "TraceId"}, rows,
		"filter-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !started || count != 2 {
		t.Fatalf("started=%v count=%d, want true and 2", started, count)
	}
	var body struct {
		Binding map[string]string `json:"binding"`
		Spans   []traces.Span     `json:"spans"`
		Next    string            `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if body.Binding["trace_id"] != "TraceId" || len(body.Spans) != 2 {
		t.Fatalf("unexpected body: %#v", body)
	}
	if want := encodeCursor(next, "filter-hash"); body.Next != want {
		t.Fatalf("next_cursor=%q, want %q", body.Next, want)
	}
}

func TestWriteSpanPageReportsEarlyAndLateReadFailures(t *testing.T) {
	t.Run("before response starts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		_, started, err := writeSpanPage(rec, map[string]string{},
			&stubSpanReader{err: errors.New("source failed")}, "")
		if err == nil || started || rec.Body.Len() != 0 {
			t.Fatalf("err=%v started=%v body=%q", err, started, rec.Body.String())
		}
	})

	t.Run("after response starts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		_, started, err := writeSpanPage(rec, map[string]string{}, &stubSpanReader{
			spans: []traces.Span{{"TraceId": "trace-1"}},
			err:   errors.New("source failed"),
		}, "")
		if err == nil || !started {
			t.Fatalf("err=%v started=%v", err, started)
		}
		if json.Valid(rec.Body.Bytes()) {
			t.Fatalf("a partial stream must not look successful: %s", rec.Body.String())
		}
	})
}

func TestWriteSpanPageWritesEachRowBeforeReadingTheNext(t *testing.T) {
	wrote := false
	rec := httptest.NewRecorder()
	w := observingWriter{ResponseRecorder: rec, wrote: &wrote}
	count, _, err := writeSpanPage(w, map[string]string{}, &orderedSpanReader{wrote: &wrote}, "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("count=%d code=%d body=%q", count, rec.Code, rec.Body.String())
	}
}

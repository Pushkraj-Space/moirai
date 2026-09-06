package cloud

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	moirai "github.com/october-dev/moirai"
)

func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	p, ok := s.readable(w, r)
	if !ok {
		return
	}
	archive, err := s.Blobs.Get(r.Context(), p.ID)
	if err != nil {
		s.fail(w, 503, "archive_unavailable")
		return
	}
	// Forks are independent publications of the immutable checkpoint. Import into
	// a harness assigns fresh native session identity using existing CLI behavior.
	body, err := json.Marshal(PublishRequest{Archive: archive, Visibility: "private", Parent: p.ID, Expires: p.Expires})
	if err != nil {
		s.fail(w, 500, "internal_error")
		return
	}
	copy := r.Clone(r.Context())
	copy.Body = io.NopCloser(bytes.NewReader(body))
	copy.ContentLength = int64(len(body))
	if _, err = moirai.DecodeArchive(archive, moirai.DefaultLimits()); err != nil {
		s.fail(w, 503, "archive_integrity_error")
		return
	}
	s.publish(w, copy)
}

package server

import (
	"io"
	"net/http"

	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/providers"
)

// maxAudioBytes bounds an uploaded audio file. The database has no blob
// storage, so the payload is buffered in memory and forwarded; anything much
// larger than a long recording would exhaust memory under concurrency.
const maxAudioBytes = 100 << 20 // 100 MiB

// audioFields are the multipart form values forwarded to a transcription
// provider beyond the file itself.
var audioFields = []string{"response_format", "language", "temperature", "timestamp_granularities", "prompt"}

// parseAudio extracts the uploaded file and model name from a multipart
// transcription request.
func (s *Server) parseAudio(r *http.Request) (*providers.AudioFile, map[string]string, *httpx.Error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxAudioBytes)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, nil, httpx.BadRequest("expected a multipart/form-data upload with a 'file' field")
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, nil, httpx.BadRequest("field 'file' is required")
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, httpx.BadRequest("could not read the uploaded file")
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	fields := map[string]string{}
	for _, name := range audioFields {
		if v := r.FormValue(name); v != "" {
			fields[name] = v
		}
	}
	if v := r.FormValue("model"); v != "" {
		fields["model"] = v
	}

	return &providers.AudioFile{
		Filename:    header.Filename,
		ContentType: contentType,
		Data:        data,
	}, fields, nil
}

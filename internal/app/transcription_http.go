package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	maxTranscriptionBody     = 25 << 20
	maxTranscriptionLanguage = 32
	transcriptionOriginator  = "Codex Desktop"
	transcriptionUserAgent   = "Codex Desktop/0.163.0 (Mac OS 27.0.0; arm64) unknown (Codex Desktop; 0.0.0)"
)

var transcriptionExtension = regexp.MustCompile(`^\.[a-z0-9]{1,4}$`)

type transcriptionUpload struct {
	body        []byte
	contentType string
	audioBytes  int64
}

func (s *server) transcribe(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(httpResponseIOWait))
	if _, authorized := s.authorizeAPIKey(r); !authorized {
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	upload, err := readTranscriptionUpload(w, r)
	if err != nil {
		status, message := http.StatusBadRequest, err.Error()
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status, message = http.StatusRequestEntityTooLarge, "audio upload exceeds 25 MiB"
		}
		w.Header().Set("Connection", "close")
		writeHTTPResponseError(w, status, message)
		return
	}
	controller.SetReadDeadline(time.Time{})
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), upstreamWait)
	defer cancel()
	skip := map[string]bool{}
	reauthed := map[string]bool{}
	var rejection *http.Response
	defer func() {
		if rejection != nil {
			rejection.Body.Close()
		}
	}()
	var retryAccount *Account
	for range 2 * len(s.pool.all()) {
		account := retryAccount
		retryAccount = nil
		if account != nil {
			candidate := account.routingCandidate()
			now := time.Now()
			if s.pool.find(candidate.id) != account || !candidate.available(now) && !candidate.creditAvailable(now) {
				account = nil
			}
		}
		if account == nil {
			account = s.pool.route(nil, skip).account
		}
		if account == nil {
			break
		}
		id := account.id()
		account.accepted(time.Now())
		resp, err := s.forwardTranscription(ctx, account, upload)
		if err != nil {
			failure := httpTransportFailure(ctx, err)
			s.log.Warn("transcription upstream failed", "account", id, "audio_bytes", upload.audioBytes, "error_class", failure.Code)
			writeHTTPResponseError(w, failure.Status, failure.Message)
			return
		}
		if resp.Header.Get("Cf-Mitigated") == "challenge" {
			resp.Body.Close()
			s.log.Warn("transcription upstream challenged the client identity", "account", id, "status", resp.StatusCode)
			writeHTTPResponseError(w, http.StatusBadGateway, "upstream challenged the balancer's transcription client identity")
			return
		}
		account.observe(resp.Header)
		usageLimit := (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden) && responseUsageLimitReached(resp)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !reauthed[id]:
			resp.Body.Close()
			reauthed[id] = true
			if !s.refreshedContext(ctx, account, id) {
				skip[id] = true
			} else {
				retryAccount = account
			}
			continue
		case usageLimit || resp.StatusCode == http.StatusTooManyRequests:
			if usageLimit {
				account.rejectCredits()
				account.markSpent()
			} else {
				account.rateLimited(resp.Header)
			}
			if s.stats != nil {
				s.stats.rateLimited(id)
			}
			if rejection != nil {
				rejection.Body.Close()
			}
			rejection = resp
			skip[id] = true
			continue
		}
		s.log.Debug("transcription proxied", "account", id, "status", resp.StatusCode, "audio_bytes", upload.audioBytes, "elapsed_ms", time.Since(started).Milliseconds())
		s.writeTranscription(w, resp)
		return
	}
	if rejection != nil {
		s.writeToolResponse(w, rejection)
		return
	}
	w.Header().Set("Retry-After", "1")
	writeHTTPResponseError(w, http.StatusServiceUnavailable, "no eligible account for transcription; retry")
}

func readTranscriptionUpload(w http.ResponseWriter, r *http.Request) (transcriptionUpload, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptionBody)
	reader, err := r.MultipartReader()
	if err != nil {
		return transcriptionUpload{}, errors.New("request must be multipart/form-data")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	var audioBytes int64
	fileSeen := false
	language := ""
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return transcriptionUpload{}, err
		}
		switch part.FormName() {
		case "file":
			if fileSeen {
				return transcriptionUpload{}, errors.New("multipart field file must appear once")
			}
			fileSeen = true
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": transcriptionFilename(part.FileName())}))
			header.Set("Content-Type", cmp.Or(part.Header.Get("Content-Type"), "application/octet-stream"))
			file, err := form.CreatePart(header)
			if err != nil {
				return transcriptionUpload{}, err
			}
			if audioBytes, err = io.Copy(file, part); err != nil {
				return transcriptionUpload{}, err
			}
		case "language":
			value, err := io.ReadAll(io.LimitReader(part, maxTranscriptionLanguage+1))
			if err != nil {
				return transcriptionUpload{}, err
			}
			if len(value) > maxTranscriptionLanguage {
				return transcriptionUpload{}, errors.New("multipart field language is too long")
			}
			language = strings.TrimSpace(string(value))
		}
	}
	if audioBytes == 0 {
		return transcriptionUpload{}, errors.New("multipart field file is required and must not be empty")
	}
	if language != "" {
		form.WriteField("language", language)
	}
	form.Close()
	return transcriptionUpload{body: body.Bytes(), contentType: form.FormDataContentType(), audioBytes: audioBytes}, nil
}

func transcriptionFilename(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if !transcriptionExtension.MatchString(ext) {
		ext = ".webm"
	}
	return "codex" + ext
}

func (s *server) forwardTranscription(ctx context.Context, account *Account, upload transcriptionUpload) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.upstream, "/codex")+"/transcribe", bytes.NewReader(upload.body))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	state := account.persisted()
	req.Header.Set("Authorization", "Bearer "+state.AccessToken)
	req.Header.Set("Chatgpt-Account-Id", claimsFromToken(state.IDToken).Auth.AccountID)
	req.Header.Set("Content-Type", upload.contentType)
	req.Header.Set("Originator", transcriptionOriginator)
	req.Header.Set("User-Agent", transcriptionUserAgent)
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(req)
}

func (s *server) writeTranscription(w http.ResponseWriter, resp *http.Response) {
	if resp.StatusCode != http.StatusOK {
		s.writeToolResponse(w, resp)
		return
	}
	defer resp.Body.Close()
	var result struct {
		Text *string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTranscriptionBody)).Decode(&result); err != nil || result.Text == nil {
		writeHTTPResponseError(w, http.StatusBadGateway, "invalid upstream transcription response")
		return
	}
	if id := resp.Header.Get("X-Request-Id"); id != "" {
		w.Header().Set("X-Request-Id", id)
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": *result.Text})
}

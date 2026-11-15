package cmd

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pngBytes is a real PNG signature plus a byte that is not valid UTF-8 on its
// own, so a download that decoded or re-encoded the body would corrupt it
// visibly rather than passing by luck.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0x00, 0x42}

const attachmentsListPath = "/wiki/api/v2/pages/123456/attachments"

// attachment is one entry of the listing a test serves. id defaults to
// att<n>, n being its 1-based position, and version to 1; noID and a negative
// version leave the field out of the listing.
type attachment struct {
	id        string
	noID      bool
	title     string
	mediaType string
	size      int
	version   int
}

// attachmentBytes is what a download pinned to a version serves: distinct per
// attachment and version, so a test can tell which one was written where.
func attachmentBytes(id, version string) []byte {
	return append(slices.Clone(pngBytes), []byte(id+"@v"+version)...)
}

// attachmentsAPI answers page 123456's listing; see pageAttachmentsAPI.
func attachmentsAPI(t *testing.T, attachments ...attachment) (*int, http.HandlerFunc) {
	t.Helper()
	return pageAttachmentsAPI(t, "123456", attachments...)
}

// pageAttachmentsAPI answers one page's listing with the given attachments and
// every download with attachmentBytes for the version it names, counting the
// downloads. A download that names no version fails the test: it would fetch
// whatever is latest rather than what the listing described.
//
// Both paths are matched exactly, so a request the command should not be
// making at all fails the test rather than being quietly answered. The
// listing hands out download links in the API's own shape.
func pageAttachmentsAPI(t *testing.T, pageID string, attachments ...attachment) (*int, http.HandlerFunc) {
	t.Helper()

	listPath := "/wiki/api/v2/pages/" + pageID + "/attachments"
	downloadBase := "/wiki/rest/api/content/" + pageID + "/child/attachment/"

	downloads := 0
	return &downloads, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == listPath:
			results := make([]map[string]any, 0, len(attachments))
			for i, a := range attachments {
				id := cmp.Or(a.id, fmt.Sprintf("att%d", i+1))
				entry := map[string]any{
					"id":           id,
					"title":        a.title,
					"mediaType":    a.mediaType,
					"fileSize":     a.size,
					"downloadLink": "/rest/api/content/" + pageID + "/child/attachment/" + id + "/download",
				}
				if a.version >= 0 {
					entry["version"] = map[string]any{"number": max(a.version, 1)}
				}
				if a.noID {
					delete(entry, "id")
				}
				results = append(results, entry)
			}
			payload, err := json.Marshal(map[string]any{"results": results, "_links": map[string]any{}})
			if err != nil {
				t.Errorf("Marshal() error = %v", err)
				return
			}
			_, _ = w.Write(payload)
		case strings.HasPrefix(r.URL.Path, downloadBase) && strings.HasSuffix(r.URL.Path, "/download"):
			downloads++
			version := r.URL.Query().Get("version")
			if version == "" {
				t.Errorf("download of %s names no version", r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, downloadBase), "/download")
			_, _ = w.Write(attachmentBytes(id, version))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}
}

func runAttachmentsListCmd(t *testing.T, arg string, limit int, extra ...string) (cliRun, error) {
	t.Helper()
	return runLimitCmd(t, "confluence attachments list", arg, limit, extra...)
}

func TestAttachmentsListShowsFilenameMediaTypeAndSize(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t,
		attachment{title: "screenshot.png", mediaType: "image/png", size: 46982},
		attachment{title: "spec.pdf", mediaType: "application/pdf", size: 3841203},
	)
	startAPI(t, handler)

	run, err := runCLI(t, "confluence", "attachments", "list", testPageURL)
	if err != nil {
		t.Fatalf("attachments list error = %v", err)
	}

	for _, want := range []string{
		"screenshot.png", "image/png", "46982",
		"spec.pdf", "application/pdf", "3841203",
	} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("output = %q, want it to contain %q", run.stdout, want)
		}
	}
	// The API's attachment id addresses the download and disambiguates
	// nothing in the listing, so it is not shown.
	if strings.Contains(run.stdout, "att1") {
		t.Errorf("output = %q, want the attachment id left out", run.stdout)
	}
}

func TestAttachmentsListJSONOutput(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t, attachment{title: "screenshot.png", mediaType: "image/png", size: 46982})
	startAPI(t, handler)

	run, err := runCLI(t, "confluence", "attachments", "list", testPageURL, "--format", "json")
	if err != nil {
		t.Fatalf("attachments list error = %v", err)
	}

	var got struct {
		Attachments []struct {
			Filename  string `json:"filename"`
			MediaType string `json:"media_type"`
			FileSize  int64  `json:"file_size"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(run.stdout), &got); err != nil {
		t.Fatalf("Unmarshal(output) error = %v; output = %q", err, run.stdout)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want the single attachment", got.Attachments)
	}
	if a := got.Attachments[0]; a.Filename != "screenshot.png" || a.MediaType != "image/png" || a.FileSize != 46982 {
		t.Errorf("attachment = %+v, want screenshot.png/image/png/46982", a)
	}
}

func TestAttachmentsListWithNoAttachments(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t)
	startAPI(t, handler)

	run, err := runCLI(t, "confluence", "attachments", "list", testPageURL)
	if err != nil {
		t.Fatalf("attachments list error = %v, want a page with no attachments to succeed", err)
	}
	if !strings.Contains(run.stdout, "No attachments.") {
		t.Errorf("output = %q, want it to say there are none", run.stdout)
	}
}

func TestAttachmentsListReportsTruncationWithoutACount(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t,
		attachment{title: "one.png", mediaType: "image/png", size: 1},
		attachment{title: "two.png", mediaType: "image/png", size: 2},
	)
	startAPI(t, handler)

	run, err := runAttachmentsListCmd(t, testPageURL, 1)
	if err != nil {
		t.Fatalf("attachments list error = %v", err)
	}
	if !strings.Contains(run.stdout, "--limit") {
		t.Errorf("output = %q, want a truncation notice pointing at --limit", run.stdout)
	}
	// v2 pages by cursor and reports no total, so no count is claimed.
	if strings.Contains(run.stdout, "1 more") {
		t.Errorf("output = %q, want no invented remaining count", run.stdout)
	}
	if strings.Contains(run.stdout, "two.png") {
		t.Errorf("output = %q, want only the first attachment at --limit 1", run.stdout)
	}
}

func TestAttachmentsListAcceptsABarePageID(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	var gotPath string
	startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"results":[],"_links":{}}`))
	})

	if _, err := runCLI(t, "confluence", "attachments", "list", "123456"); err != nil {
		t.Fatalf("attachments list error = %v", err)
	}
	if gotPath != "/wiki/api/v2/pages/123456/attachments" {
		t.Errorf("path = %q, want the page id from the bare argument", gotPath)
	}
}

func TestAttachmentsListRejectsAnOutOfRangeLimit(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	startAPI(t, neverCalled(t, "despite an invalid --limit"))

	if _, err := runAttachmentsListCmd(t, testPageURL, 0); err == nil {
		t.Error("attachments list error = nil for --limit 0, want an error")
	}
}

// runDownload runs `confluence attachments download` with --pattern and -o set.
func runDownload(t *testing.T, dir, pattern string, extra ...string) (cliRun, error) {
	t.Helper()

	args := append([]string{"confluence", "attachments", "download", testPageURL, "--pattern", pattern, "-o", dir}, extra...)
	return runCLI(t, args...)
}

func TestAttachmentsDownloadWritesMatchingFilesUnchanged(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	downloads, handler := attachmentsAPI(t,
		attachment{title: "screenshot.png", mediaType: "image/png", size: len(pngBytes)},
		attachment{title: "diagram.png", mediaType: "image/png", size: len(pngBytes)},
		attachment{title: "spec.pdf", mediaType: "application/pdf", size: 9},
	)
	startAPI(t, handler)

	// A directory that does not exist yet, so `-o ./assets` works without the
	// caller having to make it first.
	dir := filepath.Join(t.TempDir(), "assets")
	run, err := runDownload(t, dir, "*.png")
	if err != nil {
		t.Fatalf("attachments download error = %v", err)
	}

	for _, f := range []struct{ name, id string }{{"screenshot.png", "att1"}, {"diagram.png", "att2"}} {
		assertFileBytes(t, filepath.Join(dir, f.name), attachmentBytes(f.id, "1"))
		if !strings.Contains(run.stdout, f.name) {
			t.Errorf("output = %q, want it to name %s", run.stdout, f.name)
		}
	}
	// The PDF matched nothing, so it must not have been fetched: the whole
	// point of the required --pattern is not pulling what was not asked for.
	if _, err := os.Stat(filepath.Join(dir, "spec.pdf")); !os.IsNotExist(err) {
		t.Errorf("Stat(spec.pdf) error = %v, want the non-matching attachment left alone", err)
	}
	if *downloads != 2 {
		t.Errorf("downloads = %d, want 2 (only the matches)", *downloads)
	}
}

func TestAttachmentsDownloadJSONOutput(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t, attachment{title: "screenshot.png", mediaType: "image/png", size: len(pngBytes)})
	startAPI(t, handler)

	dir := filepath.Join(t.TempDir(), "assets")
	run, err := runDownload(t, dir, "*.png", "--format", "json")
	if err != nil {
		t.Fatalf("attachments download error = %v", err)
	}

	var got struct {
		Downloaded []struct {
			Filename string `json:"filename"`
			Path     string `json:"path"`
			Bytes    int64  `json:"bytes"`
		} `json:"downloaded_attachments"`
	}
	if err := json.Unmarshal([]byte(run.stdout), &got); err != nil {
		t.Fatalf("Unmarshal(output) error = %v; output = %q", err, run.stdout)
	}
	if len(got.Downloaded) != 1 {
		t.Fatalf("downloaded_attachments = %+v, want the single file", got.Downloaded)
	}
	want := filepath.Join(dir, "screenshot.png")
	wantBytes := int64(len(attachmentBytes("att1", "1")))
	if d := got.Downloaded[0]; d.Filename != "screenshot.png" || d.Path != want || d.Bytes != wantBytes {
		t.Errorf("downloaded = %+v, want screenshot.png at %s with %d bytes", d, want, wantBytes)
	}
}

func TestAttachmentsDownloadDefaultsToTheWorkingDirectory(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t, attachment{title: "screenshot.png", mediaType: "image/png", size: len(pngBytes)})
	startAPI(t, handler)

	dir := t.TempDir()
	t.Chdir(dir)

	if _, err := runCLI(t, "confluence", "attachments", "download", testPageURL, "--pattern", "*.png"); err != nil {
		t.Fatalf("attachments download error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "screenshot.png")); err != nil {
		t.Errorf("Stat(screenshot.png) error = %v, want the file in the working directory", err)
	}
}

func TestAttachmentsDownloadRefusesToOverwriteBeforeFetchingAnything(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	downloads, handler := attachmentsAPI(t,
		attachment{title: "diagram.png", mediaType: "image/png", size: len(pngBytes)},
		attachment{title: "screenshot.png", mediaType: "image/png", size: len(pngBytes)},
	)
	startAPI(t, handler)

	dir := t.TempDir()
	existing := filepath.Join(dir, "screenshot.png")
	if err := os.WriteFile(existing, []byte("mine"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := runDownload(t, dir, "*.png"); err == nil {
		t.Fatal("attachments download error = nil, want a refusal to overwrite")
	} else if !strings.Contains(err.Error(), "screenshot.png") {
		t.Errorf("error = %q, want it to name the colliding file", err)
	}
	got, readErr := os.ReadFile(existing)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(got) != "mine" {
		t.Errorf("screenshot.png = %q, want it untouched", got)
	}
	// The collision is found before any transfer, so the attachment that
	// would have succeeded is not written either — a half-applied download is
	// worse than none.
	if _, err := os.Stat(filepath.Join(dir, "diagram.png")); !os.IsNotExist(err) {
		t.Errorf("Stat(diagram.png) error = %v, want nothing written at all", err)
	}
	if *downloads != 0 {
		t.Errorf("downloads = %d, want 0 requests once a collision is known", *downloads)
	}
}

func TestAttachmentsDownloadRefusesAttachmentsThatWouldBeOneFile(t *testing.T) {
	tests := []struct {
		name   string
		titles []string
	}{
		{
			// One file on a case-insensitive filesystem.
			name:   "titles differing only in case",
			titles: []string{"a.png", "A.png"},
		},
		{
			// NFC and NFD spellings of が, one file on APFS.
			name:   "titles differing only in Unicode normalisation",
			titles: []string{"が.png", "が.png"},
		},
		{
			// One group, not one entry per pair.
			name:   "three titles on one file",
			titles: []string{"ab.png", "Ab.png", "AB.png"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			seedProfile(t, "example", testSite)
			attachments := []attachment{{title: "other.png", mediaType: "image/png", size: len(pngBytes)}}
			for _, title := range tt.titles {
				attachments = append(attachments, attachment{title: title, mediaType: "image/png", size: len(pngBytes)})
			}
			downloads, handler := attachmentsAPI(t, attachments...)
			startAPI(t, handler)

			dir := filepath.Join(t.TempDir(), "assets")
			_, err := runDownload(t, dir, "*.png")
			if err == nil {
				t.Fatal("attachments download error = nil, want the collision refused")
			}
			if group := strings.Join(tt.titles, " and "); !strings.Contains(err.Error(), group) {
				t.Errorf("error = %q, want it to name the group %q", err, group)
			}
			if *downloads != 0 {
				t.Errorf("downloads = %d, want 0 requests once a collision is known", *downloads)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("Stat(-o) error = %v, want nothing written, not even the directory", err)
			}
		})
	}
}

func TestAttachmentsDownloadWithNoMatchFails(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t, attachment{title: "spec.pdf", mediaType: "application/pdf", size: 9})
	startAPI(t, handler)

	dir := t.TempDir()
	if _, err := runDownload(t, dir, "*.png"); err == nil {
		t.Fatal("attachments download error = nil, want a non-matching pattern to fail")
	} else if !strings.Contains(err.Error(), "*.png") {
		t.Errorf("error = %q, want it to name the pattern", err)
	}
}

func TestAttachmentsDownloadOnAPageWithNoAttachmentsFails(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t)
	startAPI(t, handler)

	if _, err := runDownload(t, t.TempDir(), "*"); err == nil {
		t.Fatal("attachments download error = nil, want nothing to match on a page with no attachments")
	}
}

func TestAttachmentsDownloadRequiresThePatternFlag(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	startAPI(t, neverCalled(t, "despite a missing --pattern"))

	_, err := runCLI(t, "confluence", "attachments", "download", testPageURL)
	if err == nil {
		t.Fatal("attachments download error = nil, want the missing flag reported")
	}
	if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("error = %q, want it to name --pattern", err)
	}
}

func TestAttachmentsDownloadRejectsAMalformedPattern(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := attachmentsAPI(t, attachment{title: "screenshot.png", mediaType: "image/png", size: 1})
	startAPI(t, handler)

	// An unterminated character class. Left unchecked this looks exactly like
	// a pattern that matched nothing, which is a different problem.
	if _, err := runDownload(t, t.TempDir(), "[png"); err == nil {
		t.Fatal("attachments download error = nil, want a malformed pattern reported")
	} else if !strings.Contains(err.Error(), "[png") {
		t.Errorf("error = %q, want it to name the bad pattern", err)
	}
}

func TestAttachmentsDownloadRejectsAFilenameThatEscapesTheOutputDirectory(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	// The title is server data. A path in it would put the write outside -o.
	downloads, handler := attachmentsAPI(t, attachment{title: "../escaped", mediaType: "text/plain", size: 1})
	startAPI(t, handler)

	dir := filepath.Join(t.TempDir(), "assets")
	// The pattern has to actually match, or the run ends at "no attachment
	// matches" and never reaches the guard this test is about. path.Match's *
	// does not cross a slash, so "*escaped*" would not match "../escaped" —
	// which is also why a title like this is unreachable via a plain "*".
	_, err := runDownload(t, dir, "*/escaped")
	if err == nil {
		t.Fatal("attachments download error = nil, want a filename outside -o refused")
	}
	if !strings.Contains(err.Error(), "not a plain filename") {
		t.Fatalf("error = %q, want the path guard to reject it (not a no-match)", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "..", "escaped")); !os.IsNotExist(statErr) {
		t.Errorf("Stat(../escaped) error = %v, want nothing written outside -o", statErr)
	}
	if *downloads != 0 {
		t.Errorf("downloads = %d, want the guard to refuse before any transfer", *downloads)
	}
}

const uploadPath = "/wiki/rest/api/content/123456/child/attachment"

// uploadPart is one part of the multipart body the server received.
type uploadPart struct {
	name, filename, content string
}

// uploadRequest is what the server saw of one upload.
type uploadRequest struct {
	method, path, user, token string
	parts                     []uploadPart
}

// withAttachmentListing answers the page's attachment listing with pages, one
// response per element and each linking to the next by cursor, counts the
// listing requests, and passes every other request to next. A nil pages is a
// page holding no attachments.
func withAttachmentListing(t *testing.T, pages [][]string, next http.HandlerFunc) (*int, http.HandlerFunc) {
	t.Helper()

	if len(pages) == 0 {
		pages = [][]string{nil}
	}
	listings := 0
	return &listings, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != attachmentsListPath {
			next(w, r)
			return
		}
		listings++

		i := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil || n >= len(pages) {
				t.Errorf("listing cursor = %q, want one this listing handed out", cursor)
				return
			}
			i = n
		}
		results := []map[string]any{}
		links := map[string]any{}
		for _, title := range pages[i] {
			results = append(results, map[string]any{"title": title})
		}
		if i+1 < len(pages) {
			links["next"] = fmt.Sprintf("%s?cursor=%d", strings.TrimPrefix(attachmentsListPath, "/wiki"), i+1)
		}
		payload, err := json.Marshal(map[string]any{"results": results, "_links": links})
		if err != nil {
			t.Errorf("Marshal() error = %v", err)
			return
		}
		_, _ = w.Write(payload)
	}
}

// uploadAPI answers every upload with a result per file part it received,
// at version 1 under POST and 2 under PUT, and records the requests. The
// page's attachment listing, which an upload without --replace reads first,
// holds nothing; withAttachmentListing around it serves a different one.
func uploadAPI(t *testing.T) (*[]uploadRequest, http.HandlerFunc) {
	t.Helper()

	var requests []uploadRequest
	_, handler := withAttachmentListing(t, nil, func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		req := uploadRequest{
			method: r.Method, path: r.URL.Path, user: user,
			token: r.Header.Get("X-Atlassian-Token"),
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader() error = %v", err)
			return
		}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Errorf("NextPart() error = %v", err)
				return
			}
			content, _ := io.ReadAll(part)
			req.parts = append(req.parts, uploadPart{name: part.FormName(), filename: part.FileName(), content: string(content)})
		}
		requests = append(requests, req)

		version := 1
		if r.Method == http.MethodPut {
			version = 2
		}
		results := []map[string]any{}
		for _, p := range req.parts {
			if p.name != "file" {
				continue
			}
			results = append(results, map[string]any{
				"title":      p.filename,
				"extensions": map[string]any{"mediaType": "image/png", "fileSize": len(p.content)},
				"version":    map[string]any{"number": version},
			})
		}
		payload, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			t.Errorf("Marshal() error = %v", err)
			return
		}
		_, _ = w.Write(payload)
	})
	return &requests, handler
}

func TestAttachmentsUploadPostsTheFileUnderItsBaseName(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, handler := uploadAPI(t)
	startAPI(t, handler)

	file := writeTempFile(t, "out/a.png", string(pngBytes))

	if _, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.method != http.MethodPost || got.path != uploadPath {
		t.Errorf("request = %s %s, want POST %s", got.method, got.path, uploadPath)
	}
	if got.user != "a@example.com" {
		t.Errorf("Basic auth user = %q, want the profile's email", got.user)
	}
	if got.token != "nocheck" {
		t.Errorf("X-Atlassian-Token = %q, want nocheck", got.token)
	}
	want := []uploadPart{
		{name: "file", filename: "a.png", content: string(pngBytes)},
		{name: "minorEdit", content: "true"},
	}
	if !slices.Equal(got.parts, want) {
		t.Errorf("parts = %q, want %q", got.parts, want)
	}
}

func TestAttachmentsUploadWithReplaceIsAPut(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, handler := uploadAPI(t)
	startAPI(t, handler)

	file := writeTempFile(t, "a.png", string(pngBytes))

	if _, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file, "--replace"); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.method != http.MethodPut || got.path != uploadPath {
		t.Errorf("request = %s %s, want PUT %s", got.method, got.path, uploadPath)
	}
	want := []uploadPart{
		{name: "file", filename: "a.png", content: string(pngBytes)},
		{name: "minorEdit", content: "true"},
	}
	if got.token != "nocheck" || !slices.Equal(got.parts, want) {
		t.Errorf("token %q parts %q, want nocheck and %q — nothing but the method differs", got.token, got.parts, want)
	}
}

// TestAttachmentsUploadRefusesANameThePageHasIgnoringCase covers the check
// that keeps an upload without --replace from adding a version to an existing
// attachment: Confluence's own 400 covers an exact match only.
func TestAttachmentsUploadRefusesANameThePageHasIgnoringCase(t *testing.T) {
	tests := []struct {
		name     string
		listing  [][]string
		upload   string
		existing string
	}{
		{name: "differing in case", listing: [][]string{{"a.png"}}, upload: "A.png", existing: "a.png"},
		{
			name:    "on the listing's second page",
			listing: [][]string{{"b.png", "c.png"}, {"a.png"}},
			upload:  "A.png", existing: "a.png",
		},
		{name: "equal under full case folding", listing: [][]string{{"Straße.png"}}, upload: "STRASSE.png", existing: "Straße.png"},
		{name: "differing in normalisation", listing: [][]string{{"é.png"}}, upload: "é.png", existing: "é.png"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			seedProfile(t, "example", testSite)
			requests, upload := uploadAPI(t)
			listings, handler := withAttachmentListing(t, tt.listing, upload)
			startAPI(t, handler)

			file := writeTempFile(t, tt.upload, string(pngBytes))

			_, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file)
			if err == nil {
				t.Fatal("attachments upload error = nil, want the name refused")
			}
			for _, want := range []string{file, tt.existing, "--replace", "nothing was sent"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q in it", err, want)
				}
			}
			if len(*requests) != 0 {
				t.Errorf("uploads = %d, want none", len(*requests))
			}
			if *listings != len(tt.listing) {
				t.Errorf("listing requests = %d, want every page (%d) read", *listings, len(tt.listing))
			}
		})
	}
}

func TestAttachmentsUploadRefusesTheWholeRunNamingEveryMatch(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, upload := uploadAPI(t)
	_, handler := withAttachmentListing(t, [][]string{{"a.png", "c.png"}}, upload)
	startAPI(t, handler)

	free := writeTempFile(t, "b.png", string(pngBytes))
	first := writeTempFile(t, "A.png", string(pngBytes))
	second := writeTempFile(t, "C.png", string(pngBytes))

	_, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, free, first, second)
	if err == nil {
		t.Fatal("attachments upload error = nil, want the run refused")
	}
	for _, want := range []string{
		first + " matches the page's attachment a.png",
		second + " matches the page's attachment c.png",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q in it", err, want)
		}
	}
	if strings.Contains(err.Error(), free) {
		t.Errorf("error = %q, want %s, which matches nothing, left unnamed", err, free)
	}
	if len(*requests) != 0 {
		t.Errorf("uploads = %d, want none — not even %s", len(*requests), free)
	}
}

func TestAttachmentsUploadSendsANameThePageDoesNotHave(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, upload := uploadAPI(t)
	listings, handler := withAttachmentListing(t, [][]string{{"b.png"}}, upload)
	startAPI(t, handler)

	file := writeTempFile(t, "a.png", string(pngBytes))

	if _, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}
	if *listings != 1 || len(*requests) != 1 || (*requests)[0].method != http.MethodPost {
		t.Errorf("listings %d, uploads %+v, want the listing read and then one POST", *listings, *requests)
	}
}

func TestAttachmentsUploadWithReplaceDoesNotListTheAttachments(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, upload := uploadAPI(t)
	listings, handler := withAttachmentListing(t, [][]string{{"a.png"}}, upload)
	startAPI(t, handler)

	file := writeTempFile(t, "A.png", string(pngBytes))

	if _, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file, "--replace"); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}
	if *listings != 0 {
		t.Errorf("listing requests = %d, want none under --replace", *listings)
	}
	if len(*requests) != 1 || (*requests)[0].method != http.MethodPut {
		t.Errorf("uploads = %+v, want one PUT", *requests)
	}
}

func TestAttachmentsUploadSendsEveryFileInOneRequest(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, handler := uploadAPI(t)
	startAPI(t, handler)

	a := writeTempFile(t, "dir/a.png", string(pngBytes))
	b := writeTempFile(t, "b.png", "b")

	if _, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, a, b); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want both files in 1", len(*requests))
	}
	want := []uploadPart{
		{name: "file", filename: "a.png", content: string(pngBytes)},
		{name: "minorEdit", content: "true"},
		{name: "file", filename: "b.png", content: "b"},
		{name: "minorEdit", content: "true"},
	}
	if got := (*requests)[0].parts; !slices.Equal(got, want) {
		t.Errorf("parts = %q, want %q", got, want)
	}
}

func TestAttachmentsUploadListsWhatTheServerStored(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := uploadAPI(t)
	startAPI(t, handler)

	file := writeTempFile(t, "a.png", string(pngBytes))

	res, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file, "--replace")
	if err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}
	want := fmt.Sprintf("- **a.png** (image/png, %d bytes, version 2)\n", len(pngBytes))
	if res.stdout != want {
		t.Errorf("stdout = %q, want %q", res.stdout, want)
	}
}

func TestAttachmentsUploadJSONOutput(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	_, handler := uploadAPI(t)
	startAPI(t, handler)

	file := writeTempFile(t, "a.png", string(pngBytes))

	res, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file, "--format", "json")
	if err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}

	var got struct {
		Uploaded []map[string]any `json:"uploaded_attachments"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &got); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", res.stdout, err)
	}
	want := map[string]any{
		"filename": "a.png", "media_type": "image/png",
		"file_size": float64(len(pngBytes)), "version": float64(1),
	}
	if len(got.Uploaded) != 1 || !maps.Equal(got.Uploaded[0], want) {
		t.Errorf("uploaded_attachments = %v, want [%v]", got.Uploaded, want)
	}
}

func TestAttachmentsUploadAcceptsABarePageID(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, handler := uploadAPI(t)
	startAPI(t, handler)

	file := writeTempFile(t, "a.png", string(pngBytes))

	if _, err := runCLI(t, "confluence", "attachments", "upload", "123456", file); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}
	if len(*requests) != 1 || (*requests)[0].path != uploadPath {
		t.Errorf("requests = %+v, want one to %s", *requests, uploadPath)
	}
}

func TestAttachmentsUploadAcceptsASpaceOverviewURL(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	requests, handler := uploadAPI(t)
	startAPI(t, withSpaceLookup(t, handler))

	file := writeTempFile(t, "a.png", string(pngBytes))

	if _, err := runCLI(t, "confluence", "attachments", "upload", testSpaceOverviewURL, file); err != nil {
		t.Fatalf("attachments upload error = %v", err)
	}
	if len(*requests) != 1 || (*requests)[0].path != uploadPath {
		t.Errorf("requests = %+v, want one to the homepage's %s", *requests, uploadPath)
	}
}

func TestAttachmentsUploadPointsAtReplaceWhenThePostIsRefused(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	// The listing misses a.png, as it does for an attachment added between
	// the check and the upload.
	_, handler := withAttachmentListing(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"statusCode":400,"message":"com.atlassian.confluence.api.service.exceptions.BadRequestException: `+
			`Cannot add a new attachment with same file name as an existing attachment: a.png"}`)
	})
	startAPI(t, handler)

	file := writeTempFile(t, "a.png", string(pngBytes))

	_, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file)
	if err == nil {
		t.Fatal("attachments upload error = nil, want the refusal")
	}
	if !strings.Contains(err.Error(), "same file name as an existing attachment: a.png") {
		t.Errorf("error = %q, want the server's message", err)
	}
	if !strings.Contains(err.Error(), "--replace") {
		t.Errorf("error = %q, want it to point at --replace", err)
	}
	if code, _ := describeFailure(err, time.Minute); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestAttachmentsUploadChecksEveryFileBeforeSending(t *testing.T) {
	// Each in a directory of its own, so no path is a prefix of another and a
	// problem cannot be found in the message by way of a different one.
	dir := t.TempDir()
	valid := writeTempFile(t, "a.png", string(pngBytes))
	missing := filepath.Join(t.TempDir(), "missing.png")
	sameName := writeTempFile(t, "other/a.png", "x")
	otherCase := writeTempFile(t, "upper/A.png", "x")

	isMissing := missing
	isDir := dir + " is a directory"
	collides := valid + ", " + sameName + " would all be attached as one attachment"

	tests := []struct {
		name  string
		files []string
		// want lists one fragment per problem, each of which must appear
		// exactly once.
		want []string
	}{
		{name: "a missing file", files: []string{missing}, want: []string{isMissing}},
		{name: "a directory", files: []string{dir}, want: []string{isDir}},
		{name: "two files with the same base name", files: []string{valid, sameName}, want: []string{collides}},
		{
			name:  "two base names differing in case",
			files: []string{valid, otherCase},
			want:  []string{valid + ", " + otherCase + " would all be attached as one attachment"},
		},
		{
			name:  "one file given twice",
			files: []string{valid, valid},
			want:  []string{valid + ", " + valid + " would all be attached as one attachment"},
		},
		{name: "a valid file beside a missing one", files: []string{valid, missing}, want: []string{isMissing}},
		{
			name:  "every problem at once",
			files: []string{missing, dir, valid, sameName},
			want:  []string{isMissing, isDir, collides},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			seedProfile(t, "example", testSite)
			startAPI(t, neverCalled(t, "despite a file that cannot be uploaded"))

			args := append([]string{"confluence", "attachments", "upload", testPageURL}, tt.files...)
			_, err := runCLI(t, args...)
			if err == nil {
				t.Fatal("attachments upload error = nil, want the files rejected")
			}
			for _, fragment := range tt.want {
				if n := strings.Count(err.Error(), fragment); n != 1 {
					t.Errorf("error = %q, want %q in it once (got %d)", err, fragment, n)
				}
			}
		})
	}
}

func TestAttachmentsUploadRejectsAFileItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode of 0 does not deny reading on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	startAPI(t, neverCalled(t, "despite a file that cannot be read"))

	unreadable := writeTempFile(t, "a.png", string(pngBytes))
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	_, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, unreadable)
	if err == nil {
		t.Fatal("attachments upload error = nil, want the unreadable file rejected")
	}
	if !strings.Contains(err.Error(), "open "+unreadable) {
		t.Errorf("error = %q, want it to name %s", err, unreadable)
	}
}

func TestAttachmentsUploadRequiresAFile(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	startAPI(t, neverCalled(t, "with no file to upload"))

	if _, err := runCLI(t, "confluence", "attachments", "upload", testPageURL); err == nil {
		t.Fatal("attachments upload error = nil, want a usage error")
	}
}

// TestAttachmentsUploadReportsADeadlineThatExpiresMidUpload is the upload's
// side of exit code 124: the deadline lands while the body is still being
// sent, where the pipe the body streams through fails too, and that failure
// must not stand in for the deadline describeFailure reads.
func TestAttachmentsUploadReportsADeadlineThatExpiresMidUpload(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	// Released by the test: a server that has not read the body to its end
	// never sees the client go away, so r.Context() would not end it.
	release := make(chan struct{})
	var served atomic.Bool
	_, handler := withAttachmentListing(t, nil, func(_ http.ResponseWriter, _ *http.Request) {
		served.Store(true)
		<-release
	})
	startAPI(t, handler)
	defer close(release)

	// Large enough that the body cannot all sit in the socket buffers while
	// the server stalls without reading it.
	file := writeTempFile(t, "big.bin", string(make([]byte, 16<<20)))

	_, err := runCLI(t, "confluence", "attachments", "upload", testPageURL, file, "--timeout=300ms")

	if !served.Load() {
		t.Fatal("the request never reached the server, so the upload path was not exercised")
	}
	if code, _ := describeFailure(err, 300*time.Millisecond); code != timeoutExitCode {
		t.Errorf("exit code = %d for %v, want %d", code, err, timeoutExitCode)
	}
	// Cut short mid-body, the upload cannot have landed, so there is nothing
	// to check before running it again.
	if err != nil && strings.Contains(err.Error(), "attachments list") {
		t.Errorf("error = %q, want no may-have-landed note for a body that was not all sent", err)
	}
}

// TestAttachmentsUploadSaysWhenTheUploadMayHaveLanded covers the outcomes of
// the upload: when Confluence may have stored the files, the error names the
// check to make before uploading them again.
func TestAttachmentsUploadSaysWhenTheUploadMayHaveLanded(t *testing.T) {
	for _, fc := range creatingFailures {
		t.Run(fc.name, func(t *testing.T) {
			isolateConfig(t)
			seedProfile(t, "example", testSite)

			fail, release := failCreating(fc)
			defer release()
			_, handler := withAttachmentListing(t, nil, fail)
			startAPI(t, handler)

			_, err := runCLI(t, "confluence", "attachments", "upload", testPageURL,
				creatingBody(t, fc), fc.timeoutFlag())

			assertCreatingFailure(t, err, fc, uploadCheckHint)
			// --replace answers a 400 refusing a name the page already has,
			// not a request whose outcome is unknown.
			if fc.wantHint && strings.Contains(err.Error(), "--replace") {
				t.Errorf("error = %v, want no --replace hint", err)
			}
		})
	}
}

func TestAttachmentsListAcceptsASpaceOverviewURL(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	_, handler := attachmentsAPI(t, attachment{title: "logo.png", mediaType: "image/png", size: 3})
	startAPI(t, withSpaceLookup(t, handler))

	run, err := runAttachmentsListCmd(t, testSpaceOverviewURL, 100)
	if err != nil {
		t.Fatalf("attachments list error = %v", err)
	}
	if !strings.Contains(run.stdout, "logo.png") {
		t.Errorf("output = %q, want the homepage's attachments", run.stdout)
	}
}

func TestAttachmentsDownloadAcceptsASpaceOverviewURL(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	downloads, handler := attachmentsAPI(t, attachment{title: "logo.png", mediaType: "image/png", size: len(pngBytes)})
	startAPI(t, withSpaceLookup(t, handler))

	if _, err := runCLI(t, "confluence", "attachments", "download", testSpaceOverviewURL,
		"--pattern", "*.png", "-o", t.TempDir()); err != nil {
		t.Fatalf("attachments download error = %v", err)
	}
	if *downloads != 1 {
		t.Errorf("downloads = %d, want the homepage's one attachment", *downloads)
	}
}

// confluenceDownloadAPI answers the listing with one attachment and hands
// every download to serve, so a test controls what the transfer does.
func confluenceDownloadAPI(t *testing.T, title string, serve http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	_, serveListing := attachmentsAPI(t, attachment{title: title, mediaType: "image/png", size: len(pngBytes)})
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			serve(w, r)
			return
		}
		serveListing(w, r)
	}
}

// serveTruncated declares more bytes than it sends, so the client sees the
// connection end part-way through the body.
//
// Part-way through, it checks that nothing is at dest yet: the bytes in flight
// belong in a temporary file, so a crash at this point would leave no
// truncated file under the attachment's name.
func serveTruncated(t *testing.T, dest string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write(pngBytes)
		w.(http.Flusher).Flush()
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Errorf("Lstat(%s) mid-transfer error = %v, want nothing at the final path yet", dest, err)
		}
	}
}

// assertEmptyDir fails unless dir holds nothing: no final file and no
// temporary file left over from the transfer.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want nothing left after the failed transfer", dir, names)
	}
}

func TestAttachmentsDownloadLeavesNothingWhenATransferFailsPartWay(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)
	dir := t.TempDir()
	startAPI(t, confluenceDownloadAPI(t, "screenshot.png", serveTruncated(t, filepath.Join(dir, "screenshot.png"))))

	if _, err := runDownload(t, dir, "*.png"); err == nil {
		t.Fatal("attachments download error = nil, want the truncated transfer reported")
	}
	assertEmptyDir(t, dir)
}

// planDownloads checks every destination before the first transfer, but a file
// can still appear at one while its bytes are in flight. The move that
// publishes the download refuses an existing target, so that file survives.
func TestAttachmentsDownloadKeepsAFileThatAppearsDuringTheTransfer(t *testing.T) {
	isolateConfig(t)
	seedProfile(t, "example", testSite)

	dir := t.TempDir()
	dest := filepath.Join(dir, "screenshot.png")
	startAPI(t, confluenceDownloadAPI(t, "screenshot.png", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.WriteFile(dest, []byte("theirs"), 0o600); err != nil {
			t.Errorf("WriteFile() error = %v", err)
		}
		_, _ = w.Write(pngBytes)
	}))

	_, err := runDownload(t, dir, "*.png")
	if err == nil {
		t.Fatal("attachments download error = nil, want a refusal to replace the file that appeared")
	}
	if !strings.Contains(err.Error(), dest) {
		t.Errorf("error = %q, want it to name %s", err, dest)
	}
	assertFileBytes(t, dest, []byte("theirs"))
	entries, readDirErr := os.ReadDir(dir)
	if readDirErr != nil {
		t.Fatalf("ReadDir() error = %v", readDirErr)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file that appeared, no temporary file", len(entries))
	}
}

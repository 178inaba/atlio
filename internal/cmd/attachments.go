package cmd

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/178inaba/atlio/internal/confluence"
	"github.com/178inaba/atlio/internal/format"
	"github.com/spf13/cobra"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const defaultAttachmentsLimit = 100

func newAttachmentsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachments",
		Short: "List, download and upload a page's attachments",
	}
	cmd.AddCommand(newAttachmentsListCmd(g), newAttachmentsDownloadCmd(g), newAttachmentsUploadCmd(g))
	return cmd
}

func newAttachmentsListCmd(g *globalFlags) *cobra.Command {
	var (
		limit     int
		outFormat format.Format
	)

	cmd := &cobra.Command{
		Use:   "list <page-url|page-id>",
		Short: "List the files attached to a page",
		Long: `List a page's attachments with their filename, media type and size.

Download the ones you want with
` + "`atlio confluence attachments download`" + `: the filename shown here is what
its --pattern matches against.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttachmentsList(cmd, args, g, limit, outFormat)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", defaultAttachmentsLimit,
		"maximum number of attachments to fetch")
	addFormatFlag(cmd, &outFormat)

	return cmd
}

func newAttachmentsDownloadCmd(g *globalFlags) *cobra.Command {
	var (
		pattern   string
		outDir    string
		outFormat format.Format
	)

	cmd := &cobra.Command{
		Use:   "download <page-url|page-id> --pattern <glob>",
		Short: "Download a page's attachments matching a filename glob",
		Long: `Download the attachments whose filename matches a glob, so an image on a
page can be read with your regular file-reading tools.

--pattern is required and case-sensitive: there is no "download everything"
form, so pulling a large PDF while reaching for a screenshot has to be asked
for on purpose (--pattern '*' does it). A pattern matching nothing is an
error rather than a silent success.

An existing file is never replaced. If any attachment would overwrite one,
the command fails before downloading anything and names the file, so a run
cannot leave some files replaced and others not.

Each file is downloaded to a hidden .atlio-*.tmp file in the output directory
and moved to its name once complete, so an interrupted run leaves no partial
file under an attachment's name, and a file that appears meanwhile is not
replaced. On a filesystem that cannot rename without replacing, the name is
written directly instead, and an interrupted run can leave a partial file.

Two matching attachments that would be one file are refused the same way:
names are compared ignoring case and Unicode normalisation (a.png and A.png
are one file on a case-insensitive filesystem), on every platform.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttachmentsDownload(cmd, args, g, pattern, outDir, outFormat)
		},
	}

	// No backticks in flag usage strings: cobra reads the first backtick
	// pair as the flag's argument placeholder.
	cmd.Flags().StringVar(&pattern, "pattern", "",
		"filename glob selecting which attachments to download, e.g. *.png")
	cmd.Flags().StringVarP(&outDir, "output", "o", ".",
		"directory to write the downloaded files to, created if it does not exist")
	if err := cmd.MarkFlagRequired("pattern"); err != nil {
		panic(err)
	}
	addFormatFlag(cmd, &outFormat)

	return cmd
}

func newAttachmentsUploadCmd(g *globalFlags) *cobra.Command {
	var (
		replace   bool
		outFormat format.Format
	)

	cmd := &cobra.Command{
		Use:   "upload <page-url|page-id> <file>...",
		Short: "Attach local files to a page",
		Long: `Attach one or more regular files to a page, each under its base name
(./out/shot.png is attached as shot.png). A FIFO, a device or a directory is
refused before anything is sent.

Uploading does not change the page body, so the file does not appear on the
page yet. To show an image, read the page, add an element naming the
attachment where it should appear, and update:

  atlio confluence attachments upload <page> shot.png
  atlio confluence read <page> -o page.xml
  # add <ac:image><ri:attachment ri:filename="shot.png" /></ac:image> to page.xml
  atlio confluence update -f page.xml

A file whose name the page already has an attachment under is refused, and
then none of the files are attached. Names are compared ignoring case and
Unicode normalisation, as Confluence would otherwise take A.png as a new
version of a.png, and the page's attachments are listed to check this before
anything is sent. --replace uploads it as a new version of that attachment
instead, keeping its history, and creates the rest; for a name differing in
case that is the existing attachment, and the output reports its name. The
page shows the new version without a ` + "`confluence update`" + `, so retaking a
screenshot that is already on the page is this one command.

All files go in one request, and --timeout is the deadline for the whole
upload: raise it for a large file. The site's attachment size limit is not
checked locally; a file over it fails at the server.

A failure after the whole request was sent — the deadline expiring while the
response is awaited (exit code 124), a 5xx answer, or a 2xx that could not be
parsed (exit code 1) — may have stored the files anyway, and its error says
so. Only a 3xx or 4xx says it was not carried out, and a failure before the
whole request was sent cannot have stored anything. When the error says the
files may have been stored, check with ` + "`atlio confluence attachments list`" + `
before running it again: without --replace a repeat is refused if the files
did land, and with --replace it adds another, identical version.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttachmentsUpload(cmd, args, g, replace, outFormat)
		},
	}

	cmd.Flags().BoolVar(&replace, "replace", false,
		"add a new version to an attachment the page already has under the same name, ignoring case, instead of refusing")
	addFormatFlag(cmd, &outFormat)

	return cmd
}

// attachmentItem is one rendered attachment, whichever product holds it.
// Filename is named for what it is, whatever the API calls it (Confluence's
// title). FileSize is the server's own byte count, printed raw
// the way `confluence read` prints the size of the file it wrote.
type attachmentItem struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	FileSize  int64  `json:"file_size"`
}

func (a attachmentItem) markdown() string {
	return fmt.Sprintf("- **%s** (%s, %d bytes)", a.Filename, a.MediaType, a.FileSize)
}

func runAttachmentsList(cmd *cobra.Command, args []string, g *globalFlags, limit int, outFormat format.Format) error {
	if err := validateLimit(limit); err != nil {
		return err
	}

	ctx, cancel := commandContext(cmd, g.timeout)
	defer cancel()

	client, pageID, err := resolvePage(ctx, args[0], g.profile)
	if err != nil {
		return err
	}

	attachments, hasMore, err := client.PageAttachments(ctx, pageID, limit)
	if err != nil {
		return err
	}

	items := make([]attachmentItem, 0, len(attachments))
	for _, a := range attachments {
		items = append(items, attachmentItem{Filename: a.Title, MediaType: a.MediaType, FileSize: a.FileSize})
	}

	// v2 pages with opaque cursors and reports no total, so the notice
	// cannot say how many attachments were left out.
	notice := ""
	if hasMore {
		notice = "More attachments available; raise --limit to fetch them."
	}

	return writeList(cmd, outFormat, "attachments", items, notice)
}

// downloadedItem is one file `download` wrote. Bytes is what was actually
// written, not the size the listing reported for it.
type downloadedItem struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
}

func (d downloadedItem) markdown() string {
	return fmt.Sprintf("- %s (%d bytes)", d.Path, d.Bytes)
}

func runAttachmentsDownload(cmd *cobra.Command, args []string, g *globalFlags, pattern, outDir string, outFormat format.Format) error {
	// Hoisted ahead of the transfers rather than left to writeList. A --format
	// typo never reaches here — Format.Set rejects one as cobra parses the
	// flags — so this is the guard the writers keep against a Format built by
	// conversion instead, and this is the one command that would otherwise
	// have written files to disk before reaching it.
	if err := outFormat.Validate(); err != nil {
		return err
	}

	// One context for the listing and every transfer: --timeout is the
	// deadline for the invocation, not for each request in it.
	ctx, cancel := commandContext(cmd, g.timeout)
	defer cancel()

	client, pageID, err := resolvePage(ctx, args[0], g.profile)
	if err != nil {
		return err
	}

	// maxLimit rather than a --limit of its own: a matching attachment left
	// outside the window would be a wrong answer, not a truncated listing, and
	// there would be no flag to raise. If the page holds more than this, the
	// messages below say so instead of claiming what was not read.
	attachments, hasMore, err := client.PageAttachments(ctx, pageID, maxLimit)
	if err != nil {
		return err
	}

	files := make([]attachmentFile, 0, len(attachments))
	for _, a := range attachments {
		files = append(files, confluenceAttachmentFile(client, a))
	}
	matched, err := matchAttachments(files, pattern)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		msg := fmt.Sprintf("no attachment matches %q (%d on the page)", pattern, len(attachments))
		if hasMore {
			msg += "; the page holds more attachments than this command read"
		}
		return errors.New(msg)
	}

	planned, err := planDownloads(matched, outDir)
	if err != nil {
		return err
	}
	items, err := writeDownloads(ctx, planned, outDir)
	if err != nil {
		return err
	}

	notice := ""
	if hasMore {
		notice = "The page holds more attachments than this command read, so the match may be incomplete."
	}

	return writeList(cmd, outFormat, "downloaded attachments", items, notice)
}

// uploadedItem is one attachment `upload` left on the page, as the server
// reports it. Version tells a new attachment (1) from a replaced one, and
// MediaType is Confluence's own reading of the file, which is what decides
// whether <ac:image> renders it.
type uploadedItem struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	FileSize  int64  `json:"file_size"`
	Version   int    `json:"version"`
}

func (u uploadedItem) markdown() string {
	return fmt.Sprintf("- **%s** (%s, %d bytes, version %d)", u.Filename, u.MediaType, u.FileSize, u.Version)
}

func runAttachmentsUpload(cmd *cobra.Command, args []string, g *globalFlags, replace bool, outFormat format.Format) error {
	if err := outFormat.Validate(); err != nil {
		return err
	}

	opened, err := openUploads(args[1:])
	if err != nil {
		return err
	}
	defer closeFiles(opened)

	ctx, cancel := commandContext(cmd, g.timeout)
	defer cancel()

	client, pageID, err := resolvePage(ctx, args[0], g.profile)
	if err != nil {
		return err
	}

	// Confluence refuses only an exact match and takes a name differing in
	// case as a new version of the existing attachment, so the refusal
	// --replace promises is made here. An attachment added between this
	// listing and the upload is not caught — the API has no conditional
	// create — and an exact match among those is still refused by the 400
	// below.
	if !replace {
		existing, err := client.AllPageAttachments(ctx, pageID)
		if err != nil {
			return fmt.Errorf("list the page's attachments: %w", err)
		}
		if conflicts := existingNameConflicts(opened, existing); len(conflicts) > 0 {
			return fmt.Errorf("cannot upload (nothing was sent): %s; "+
				"pass --replace to upload a new version of that attachment instead",
				strings.Join(conflicts, "; "))
		}
	}

	files := make([]confluence.UploadFile, 0, len(opened))
	for _, f := range opened {
		files = append(files, confluence.UploadFile{Name: filepath.Base(f.Name()), Content: f})
	}

	uploaded, err := client.UploadAttachments(ctx, pageID, files, replace)
	if err != nil {
		switch {
		case errors.Is(err, confluence.ErrUploadRefused):
			return fmt.Errorf("%w; pass --replace to upload a new version of that attachment instead", err)
		case errors.Is(err, confluence.ErrUnconfirmed):
			return fmt.Errorf("%w; check with `atlio confluence attachments list` before uploading again", err)
		}
		return err
	}

	items := make([]uploadedItem, 0, len(uploaded))
	for _, u := range uploaded {
		items = append(items, uploadedItem{Filename: u.Title, MediaType: u.MediaType, FileSize: u.FileSize, Version: u.Version})
	}
	return writeList(cmd, outFormat, "uploaded attachments", items, "")
}

// existingNameConflicts describes each file whose base name the page already
// has an attachment under, as attachmentNameKey compares them.
func existingNameConflicts(files []*os.File, attachments []confluence.Attachment) []string {
	byKey := map[string][]string{}
	for _, a := range attachments {
		key := attachmentNameKey(a.Title)
		byKey[key] = append(byKey[key], a.Title)
	}

	var conflicts []string
	for _, f := range files {
		if titles := byKey[attachmentNameKey(filepath.Base(f.Name()))]; len(titles) > 0 {
			conflicts = append(conflicts, fmt.Sprintf("%s matches the page's attachment %s",
				f.Name(), strings.Join(titles, ", ")))
		}
	}
	return conflicts
}

// openUploads opens every file to upload, attached under its base name, and
// refuses the whole run — naming every offending argument — if any cannot be
// read or two would be the same attachment.
//
// Every check runs before any request, as in planDownloads, so a run never
// attaches some files and then stops at a bad one. Opening rather than only
// stat-ing is what makes "cannot be read" a local finding, and the files are
// kept open for the upload so the checks hold for what is sent.
//
// Only a regular file is accepted, and that is decided from a Stat before the
// open: opening a FIFO blocks until a writer appears, before --timeout is
// running, and a device can stream without end. SameFile on the open handle
// then refuses a path that was swapped for another file in between; a swap to
// a FIFO in that window would still block the open, which only a racing local
// process could arrange.
func openUploads(paths []string) ([]*os.File, error) {
	var (
		files    []*os.File
		problems []string
	)

	for _, p := range paths {
		f, problem := openRegular(p)
		if problem != "" {
			problems = append(problems, problem)
			continue
		}
		files = append(files, f)
	}

	sameAttachment := func(p string) string { return attachmentNameKey(filepath.Base(p)) }
	for _, same := range collidingGroups(paths, sameAttachment) {
		problems = append(problems, fmt.Sprintf("%s would all be attached as one attachment",
			strings.Join(same, ", ")))
	}

	if len(problems) > 0 {
		closeFiles(files)
		return nil, fmt.Errorf("cannot upload (nothing was sent): %s", strings.Join(problems, "; "))
	}
	return files, nil
}

// openRegular opens p for upload, or describes why it cannot be one.
func openRegular(p string) (*os.File, string) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err.Error()
	}
	switch {
	case info.IsDir():
		return nil, p + " is a directory"
	case !info.Mode().IsRegular():
		return nil, p + " is not a regular file"
	}

	f, err := os.Open(p)
	if err != nil {
		return nil, err.Error()
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err.Error()
	}
	if !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, p + " changed while it was being checked"
	}
	return f, ""
}

// closeFiles closes files opened only for reading, where a failed close loses
// nothing.
func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// matchAttachments keeps the attachments whose filename matches pattern.
//
// path.Match rather than filepath.Match: an attachment's title is a filename,
// and filepath's meaning of a backslash is platform-dependent, so the same
// pattern would select differently on Windows. Matching is case-sensitive, as
// in `gh release download`.
//
// A malformed pattern is reported rather than left to match nothing, which
// would be indistinguishable from a pattern that is merely too specific.
func matchAttachments(files []attachmentFile, pattern string) ([]attachmentFile, error) {
	var matched []attachmentFile
	for _, a := range files {
		ok, err := path.Match(pattern, a.filename)
		if err != nil {
			return nil, fmt.Errorf("invalid --pattern %q: %w", pattern, err)
		}
		if ok {
			matched = append(matched, a)
		}
	}
	return matched, nil
}

// attachmentFile is one attachment as the download paths see it, whichever
// product holds it: the filename it is matched against and reported under, and
// how to fetch its bytes.
type attachmentFile struct {
	filename string
	// id is the product's identifier for the attachment, set where it is
	// needed to tell attachments with the same filename apart.
	id string
	// saveAs is the name the file is written under when that is not its
	// filename; empty means the filename.
	saveAs string
	fetch  func(ctx context.Context, w io.Writer) (int64, error)
}

// localName is the name the attachment is written under.
func (a attachmentFile) localName() string {
	return cmp.Or(a.saveAs, a.filename)
}

// confluenceAttachmentFile adapts a page attachment to the download paths.
func confluenceAttachmentFile(client *confluence.Client, a confluence.Attachment) attachmentFile {
	return attachmentFile{
		filename: a.Title,
		fetch: func(ctx context.Context, w io.Writer) (int64, error) {
			return client.DownloadAttachment(ctx, a.DownloadLink, w)
		},
	}
}

// plannedDownload is one matched attachment together with the file it will be
// written to, so the transfer loop never has to pair the two by index.
type plannedDownload struct {
	file attachmentFile
	dest string
}

// attachmentDest resolves the file an attachment is written to under outDir.
//
// The filename comes from the server, so it is also what pins the write inside
// outDir: a filename carrying a path would otherwise land somewhere the caller
// never named. Base is the local filesystem's idea of a separator, which is
// the right one — this is a path being built, not a pattern being matched.
func attachmentDest(filename, outDir string) (string, error) {
	if !isPlainFilename(filename) {
		return "", fmt.Errorf("attachment %q is not a plain filename; "+
			"refusing to write it outside %s", filename, outDir)
	}
	return filepath.Join(outDir, filename), nil
}

// isPlainFilename reports whether a name the server supplied can be joined to
// a directory without escaping it. Every path a page names a file with is
// checked through here, so the rule cannot be applied in two versions.
func isPlainFilename(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

// localFileKey is equal for two names exactly when they would be one file on
// a filesystem that ignores case and Unicode normalisation, as macOS's default
// APFS does. It is Unicode's canonical caseless matching, NFD(fold(NFD(name))),
// whose inner NFD is what lets a decomposed spelling fold like a composed one.
// The answer is the same on every platform, so a page or issue downloads the
// same files everywhere.
func localFileKey(name string) string {
	return norm.NFD.String(cases.Fold().String(norm.NFD.String(name)))
}

// attachmentNameKey is equal for two filenames Confluence may take as one
// attachment. Confluence has been seen to match names ignoring ASCII case;
// whether it also merges names differing in full case folding or Unicode
// normalisation is unknown, so this is the wider localFileKey. Erring wide is
// the safe side for an upload: refusing a name Confluence would keep apart
// costs a --replace, while letting through one it would merge adds a version
// to an existing file.
func attachmentNameKey(name string) string {
	return localFileKey(name)
}

// collidingGroups returns, in first-appearance order, every group of two or
// more items whose key is equal.
func collidingGroups(items []string, key func(string) string) [][]string {
	byKey := map[string][]string{}
	var order []string
	for _, item := range items {
		k := key(item)
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], item)
	}

	var groups [][]string
	for _, k := range order {
		if len(byKey[k]) > 1 {
			groups = append(groups, byKey[k])
		}
	}
	return groups
}

// planDownloads resolves where each matched attachment will be written, and
// refuses the whole run if two of them would be one file or any of those files
// already exists.
//
// Checking every destination up front is what keeps the refusal from being
// half-applied: finding the collision after two of three transfers would leave
// a directory neither the caller nor a re-run can reason about. Lstat rather
// than Stat, so a dangling symlink counts as an existing entry here — which is
// what the O_EXCL create would say about it too.
//
// Two destinations of one run are compared here rather than left to the
// server's naming rules: which names are one file is a property of the local
// filesystem, and neither product documents what it lets a listing hold.
func planDownloads(matched []attachmentFile, outDir string) ([]plannedDownload, error) {
	names := make([]string, 0, len(matched))
	for _, a := range matched {
		names = append(names, a.localName())
	}
	var sameFile []string
	for _, group := range collidingGroups(names, localFileKey) {
		sameFile = append(sameFile, strings.Join(group, " and "))
	}
	if len(sameFile) > 0 {
		return nil, fmt.Errorf("%s would be written to the same file; narrow --pattern (nothing was downloaded)",
			strings.Join(sameFile, ", "))
	}

	planned := make([]plannedDownload, 0, len(matched))
	var collisions []string

	for _, a := range matched {
		dest, err := attachmentDest(a.localName(), outDir)
		if err != nil {
			return nil, err
		}

		if _, err := os.Lstat(dest); err == nil {
			collisions = append(collisions, dest)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("check %s: %w", dest, err)
		}
		planned = append(planned, plannedDownload{file: a, dest: dest})
	}

	if len(collisions) > 0 {
		return nil, fmt.Errorf("refusing to overwrite %s; delete them or pass a different -o "+
			"(nothing was downloaded)", strings.Join(collisions, ", "))
	}
	return planned, nil
}

// writeDownloads creates outDir and writes every planned download into it,
// stopping at the first failure.
func writeDownloads(ctx context.Context, planned []plannedDownload, outDir string) ([]downloadedItem, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", outDir, err)
	}

	items := make([]downloadedItem, 0, len(planned))
	for _, p := range planned {
		n, err := downloadToFile(ctx, p.file, p.dest)
		if err != nil {
			return nil, err
		}
		items = append(items, downloadedItem{Filename: p.file.filename, Path: p.dest, Bytes: n})
	}
	return items, nil
}

// downloadToFile streams one attachment to dest and reports how many bytes it
// wrote.
//
// The bytes go to a temporary file beside dest and are moved into place only
// once the transfer and the close have succeeded, so a transfer cut short — by
// a failure, a signal or a crash — never leaves a truncated file at a name that
// looks complete; at most a temporary file is left behind. The move refuses an
// existing target: the callers have already ruled out a collision, and this is
// what keeps a file that appeared in between from being replaced anyway.
//
// An error wraps fs.ErrExist exactly when dest itself already exists, and
// callers branch on that: the download commands refuse the file, and
// fetchAttachment links it as the attachment version its path names.
func downloadToFile(ctx context.Context, file attachmentFile, dest string) (int64, error) {
	f, err := createTemp(filepath.Dir(dest))
	if err != nil {
		return 0, err
	}
	tmp := f.Name()

	n, err := file.fetch(ctx, f)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, err
	}
	// Closed explicitly as well, so a flush failure is reported rather than
	// silently swallowed.
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("finalize %s: %w", dest, err)
	}
	if err := publishFile(tmp, dest, renameNoReplace); err != nil {
		return 0, err
	}
	return n, nil
}

// createTemp creates a hidden file in dir for a download in flight.
//
// The name is a fixed pattern rather than one derived from the attachment's,
// so a filename near the filesystem's length limit still gets one. Created
// with the mode a download ends up with, which os.CreateTemp's 0600 is not.
//
// A collision is retried, and one that persists is reported without wrapping
// fs.ErrExist: that error is downloadToFile's way of saying dest exists, and a
// temporary name that happened to be taken says nothing about dest.
func createTemp(dir string) (*os.File, error) {
	const attempts = 3
	for range attempts {
		f, err := os.OpenFile(filepath.Join(dir, ".atlio-"+rand.Text()+".tmp"),
			os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create a temporary file in %s: %w", dir, err)
		}
	}
	return nil, fmt.Errorf("create a temporary file in %s: %d names were already taken", dir, attempts)
}

// publishFile moves the finished download at tmp to dest without replacing
// anything at dest, and removes tmp whatever happens.
//
// move is the operating system's no-replace rename. Where the filesystem has
// none, the bytes are copied into dest created with O_EXCL instead: dest is
// still never replaced, but a copy cut short leaves a truncated dest, which is
// the guarantee such a filesystem cannot give. move is a parameter so the
// fallback can be exercised on a filesystem that does support the rename.
func publishFile(tmp, dest string, move func(oldpath, newpath string) error) error {
	defer func() { _ = os.Remove(tmp) }()

	err := move(tmp, dest)
	if errors.Is(err, errors.ErrUnsupported) {
		err = copyExclusive(tmp, dest)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	return nil
}

// copyExclusive copies src into dest, creating dest with O_EXCL. A copy that
// fails takes the partial dest with it.
func copyExclusive(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		return err
	}
	return nil
}

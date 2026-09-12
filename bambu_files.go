package main

// Finding the sliced file a Bambu print is running from.
//
// X2-series printers never name the file they are printing. gcode_file is an
// internal path (/data/Metadata/plate_1.gcode) that FTPS cannot reach, since
// FTPS serves only the removable drive, and subtask_name is the project's title
// rather than a filename: Bambu Studio rewrites it when uploading (spaces to
// underscores, slashes dropped) and is known to reuse a cached project name, so
// several files on one drive routinely share a title. A1-class printers do name
// their file, and that path is unchanged.
//
// So a filename is only ever a hint. The file is identified by matching what it
// contains against what the printer reports about the running job: the layer
// count of the plate being printed, and the material each used filament was
// sliced for versus what is loaded in the tray feeding it. A name hint is tried
// first to avoid reading the whole drive, but it is verified the same way as any
// other candidate, because a cached name can point at a stale file from an
// earlier slice. Recording the wrong file's grams is worse than recording none.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jlaffaye/ftp"
)

const (
	// bambuTitlePath holds the project title inside a .3mf, and bambuPlateGcode
	// is the plate's executable gcode, whose header carries the layer count.
	bambuTitlePath  = "3D/3dmodel.model"
	bambuPlateGcode = "Metadata/plate_%d.gcode"

	// Both files are read only far enough to find one header field, rather than
	// inflating a megabyte of mesh or toolpath.
	bambuTitleScanLimit = 64 << 10
	bambuGcodeScanLimit = 256 << 10
)

// bambuSlicedFile is what one .3mf on the printer's drive says about itself.
type bambuSlicedFile struct {
	Path   string
	Title  string
	Layers int             // total layers of the plate that was inspected
	Usage  map[int]float64 // grams per slicer filament id
	Types  map[int]string  // material per slicer filament id, e.g. "PETG"
}

// bambuFileIndex remembers what each file on a printer's drive contains, keyed
// by path, size and modification time, so a reprint of something uploaded months
// ago costs one read ever rather than one per print.
type bambuFileIndex struct {
	mu    sync.Mutex
	files map[string]bambuSlicedFile
}

func newBambuFileIndex() *bambuFileIndex {
	return &bambuFileIndex{files: make(map[string]bambuSlicedFile)}
}

func (idx *bambuFileIndex) lookup(key string) (bambuSlicedFile, bool) {
	if idx == nil {
		return bambuSlicedFile{}, false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	f, ok := idx.files[key]
	return f, ok
}

func (idx *bambuFileIndex) store(key string, f bambuSlicedFile) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.files[key] = f
}

// bambuFindSlicedFile returns the file on the printer's drive that belongs to
// the running job, or an error naming what it found instead. Nothing is returned
// unless it matches the job, so a caller can trust the grams it gets back.
func bambuFindSlicedFile(ip string, port int, accessCode string, job bambuJobRef, idx *bambuFileIndex) (bambuSlicedFile, error) {
	conn, err := dialBambuFTPSPort(ip, port, accessCode)
	if err != nil {
		return bambuSlicedFile{}, err
	}
	defer func() { _ = conn.Quit() }()

	tried := make(map[string]bool)
	inspect := func(remote string, size uint64, stamp string) (bambuSlicedFile, bool) {
		if tried[remote] {
			return bambuSlicedFile{}, false
		}
		tried[remote] = true
		key := fmt.Sprintf("%s|%s|%d|%s", ip, remote, size, stamp)
		if f, ok := idx.lookup(key); ok {
			return f, true
		}
		data, err := retrBambuFile(conn, remote)
		if err != nil {
			return bambuSlicedFile{}, false
		}
		f := bambuInspectSlicedFile(remote, data, job.PlateIdx)
		idx.store(key, f)
		return f, true
	}

	// The name hint: where Bambu Studio puts a freshly sent job. Verified like
	// anything else, since the name can be a cached one pointing at an old slice.
	for _, remote := range bambuSlicedFileCandidates(job.GcodeFile, job.SubtaskName) {
		if f, ok := inspect(remote, 0, "hint"); ok && bambuFileMatchesJob(f, job) {
			return f, nil
		}
	}

	// Otherwise read the drive. Every file is considered, whatever its age or
	// name, so a reprint of an old file resolves the same as a fresh upload.
	var matches []bambuSlicedFile
	for _, dir := range []string{"/", "/" + bambuCacheDir} {
		entries, err := conn.List(dir)
		if err != nil {
			continue // an absent cache dir is normal
		}
		for _, e := range entries {
			if e.Type != ftp.EntryTypeFile || !strings.HasSuffix(strings.ToLower(e.Name), ".3mf") {
				continue
			}
			remote := strings.TrimPrefix(strings.TrimSuffix(dir, "/")+"/"+e.Name, "/")
			f, ok := inspect(remote, e.Size, e.Time.UTC().Format("20060102150405"))
			if ok && bambuFileMatchesJob(f, job) {
				matches = append(matches, f)
			}
		}
	}

	return bambuPickSlicedFile(matches, job)
}

// bambuPickSlicedFile decides between the files that match the job. The title is
// a weak signal (Studio rewrites it, and re-slices of one project share it), so
// it only ever narrows a field that is already ambiguous. Files that agree on
// the grams are interchangeable, which is what duplicate copies of one slice
// look like, so those are accepted rather than refused.
func bambuPickSlicedFile(matches []bambuSlicedFile, job bambuJobRef) (bambuSlicedFile, error) {
	switch len(matches) {
	case 0:
		return bambuSlicedFile{}, fmt.Errorf("no file on the printer's drive matches this print")
	case 1:
		return matches[0], nil
	}

	if job.SubtaskName != "" {
		var titled []bambuSlicedFile
		for _, f := range matches {
			if f.Title == job.SubtaskName {
				titled = append(titled, f)
			}
		}
		if len(titled) == 1 {
			return titled[0], nil
		}
		if len(titled) > 1 {
			matches = titled
		}
	}

	if bambuSameUsage(matches) {
		return matches[0], nil
	}

	var names []string
	for _, f := range matches {
		names = append(names, f.Path)
	}
	sort.Strings(names)
	return bambuSlicedFile{}, fmt.Errorf("%d files on the printer's drive match this print and disagree on filament usage (%s)",
		len(names), strings.Join(names, ", "))
}

// bambuSameUsage reports whether every file records the same grams per filament.
func bambuSameUsage(files []bambuSlicedFile) bool {
	for _, f := range files[1:] {
		if len(f.Usage) != len(files[0].Usage) {
			return false
		}
		for id, grams := range files[0].Usage {
			if f.Usage[id] != grams {
				return false
			}
		}
	}
	return true
}

// bambuFileMatchesJob reports whether a file could be the one being printed. It
// compares only facts the printer states about the running job, and skips any
// check the printer or the file leaves unanswered rather than guessing.
func bambuFileMatchesJob(f bambuSlicedFile, job bambuJobRef) bool {
	if len(f.Usage) == 0 {
		return false // nothing to record from this file anyway
	}
	if job.Layers > 0 && f.Layers > 0 && f.Layers != job.Layers {
		return false
	}
	// Each filament that used grams must be the material loaded in the tray the
	// printer says feeds it.
	for id := range f.Usage {
		if id < 1 || id > len(job.Mapping) {
			continue
		}
		loaded, known := job.Sources[job.Mapping[id-1]]
		if !known || loaded == "" || f.Types[id] == "" {
			continue
		}
		if !strings.EqualFold(loaded, f.Types[id]) {
			return false
		}
	}
	return true
}

// bambuInspectSlicedFile reads the three things that identify a .3mf. A file
// that fails to parse comes back empty, which no job matches.
func bambuInspectSlicedFile(remote string, data []byte, plateIdx int) bambuSlicedFile {
	f := bambuSlicedFile{Path: remote}
	usage, types, err := parseSliceInfoPlate(data, plateIdx)
	if err != nil {
		log.Printf("Bambu: %s is not a readable sliced file: %v", remote, err)
		return f
	}
	f.Usage, f.Types = usage, types
	f.Title = bambuProjectTitle(data)
	f.Layers = bambuPlateLayerCount(data, plateIdx)
	return f
}

var bambuTitleRe = regexp.MustCompile(`<metadata name="Title">([^<]*)</metadata>`)

// bambuProjectTitle reads the project title a printer reports as subtask_name.
func bambuProjectTitle(threemf []byte) string {
	head, err := readZipEntryPrefix(threemf, bambuTitlePath, bambuTitleScanLimit)
	if err != nil {
		return ""
	}
	if m := bambuTitleRe.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}

var bambuLayerCountRe = regexp.MustCompile(`(?m)^;\s*total layer number:\s*(\d+)`)

// bambuPlateLayerCount reads the plate's layer count, which the printer reports
// live as total_layer_num and which separates two slices of the same model.
func bambuPlateLayerCount(threemf []byte, plateIdx int) int {
	if plateIdx < 1 {
		plateIdx = 1
	}
	head, err := readZipEntryPrefix(threemf, fmt.Sprintf(bambuPlateGcode, plateIdx), bambuGcodeScanLimit)
	if err != nil {
		return 0
	}
	if m := bambuLayerCountRe.FindSubmatch(head); m != nil {
		n, err := strconv.Atoi(string(m[1]))
		if err == nil {
			return n
		}
	}
	return 0
}

// readZipEntryPrefix inflates at most limit bytes of one entry, so a header can
// be read without expanding a megabyte of mesh or toolpath.
func readZipEntryPrefix(threemf []byte, name string, limit int64) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(threemf), int64(len(threemf)))
	if err != nil {
		return nil, fmt.Errorf("open 3mf zip: %w", err)
	}
	for _, f := range zr.File {
		if !strings.EqualFold(f.Name, name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(io.LimitReader(rc, limit))
	}
	return nil, fmt.Errorf("%s not found in 3mf", name)
}

// retrBambuFile downloads one file over an already open FTPS session, so a scan
// of the drive costs one login rather than one per file.
func retrBambuFile(conn *ftp.ServerConn, remote string) ([]byte, error) {
	r, err := conn.Retr(remote)
	if err != nil {
		return nil, fmt.Errorf("FTPS retrieve %q: %w", remote, err)
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// parseBambuSources reads what is loaded where, as a lookup from a mapping value
// to its material. nil when the report carries no filament blocks, which is how
// a delta report leaves the previous answer standing.
func parseBambuSources(payload []byte) map[int]string {
	layout, _ := parseBambuLayout(payload)
	if layout.Empty() {
		return nil
	}
	return bambuSourceMaterials(layout)
}

// bambuSourceMaterials maps a mapping value to the material loaded there, so a
// file sliced for PETG is not mistaken for the PLA slice of the same model. Both
// the flat tray numbering of single-AMS printers and the unit<<8|slot form the
// X2D uses are recorded, since only values a printer actually reports are ever
// looked up. Externals are keyed by their slot id both bare and shifted,
// covering vt_tray (A1) and vir_slot (X2D).
func bambuSourceMaterials(layout bambuLayout) map[int]string {
	out := make(map[int]string)
	set := func(key int, material string) {
		if material != "" {
			out[key] = material
		}
	}
	for _, u := range layout.Units {
		for _, t := range u.Trays {
			set(u.ID<<8|t.ID, t.Material)
			set(u.ID*4+t.ID, t.Material)
		}
	}
	for _, e := range layout.Externals {
		set(e.ID, e.Material)
		set(e.ID<<8, e.Material)
	}
	return out
}

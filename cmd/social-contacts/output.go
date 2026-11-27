package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/artipop/social-contacts-skill/record"
)

type outputFlags struct {
	path string
}

func (o *outputFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.path, "out", "",
		"дописывать JSONL в файл вместо stdout; повторный запуск с тем же файлом пропускает готовые входы")
}

// classified is what resuming needs from a line: whom it answers and what it
// means.
type classified interface {
	Outcome() record.Outcome
	Final() bool
}

// decodeLine reads one line of the output file as the command's record type.
type decodeLine func(line []byte) (source string, rec classified, err error)

func decodeTelegram(line []byte) (string, classified, error) {
	var r record.Telegram
	err := json.Unmarshal(line, &r)
	return r.Source, r, err
}

func decodeInstagram(line []byte) (string, classified, error) {
	var r record.Instagram
	err := json.Unmarshal(line, &r)
	return r.Source, r, err
}

func decodeVK(line []byte) (string, classified, error) {
	var r record.VK
	err := json.Unmarshal(line, &r)
	return r.Source, r, err
}

func decodeVKContacts(line []byte) (string, classified, error) {
	var r record.VKContacts
	err := json.Unmarshal(line, &r)
	return r.Source, r, err
}

// done is what an earlier run already settled: for each source with a final
// record, what it came to.
type done map[string]record.Outcome

// loadDone reads an existing output file. A source counts as done once any of
// its lines is final; blocked and failed inputs are tried again. A torn last
// line, from a run killed mid-write, is ignored.
func loadDone(path string, decode decodeLine, stderr io.Writer) (done, error) {
	out := done{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, usageErr("не прочитать %s: %v", path, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
	line, bad := 0, 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		source, rec, err := decode(text)
		if err != nil || source == "" {
			bad++
			continue
		}
		if !rec.Final() {
			continue
		}
		out[source] = merge(out[source], rec.Outcome())
	}
	if err := scanner.Err(); err != nil {
		return nil, usageErr("не прочитать %s: %v", path, err)
	}
	if bad > 0 {
		fmt.Fprintf(stderr, "в %s пропущено нечитаемых строк: %d\n", path, bad)
	}
	return out, nil
}

// merge combines the outcomes of several lines for one source: anything
// found wins over nothing found, which wins over an invalid link.
func merge(a, b record.Outcome) record.Outcome {
	rank := map[record.Outcome]int{
		"": 0, record.Invalid: 1, record.NotFound: 2, record.Found: 3,
		record.Failed: 4, record.Blocked: 5, record.NeedsLogin: 6,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// sink writes records, one JSON line each.
type sink struct {
	w     io.Writer
	close func() error
}

func (a *app) openSink(path string) (*sink, error) {
	if path == "" {
		return &sink{w: a.stdout, close: func() error { return nil }}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, usageErr("не открыть %s: %v", path, err)
	}
	// A run killed mid-write leaves a line without its newline; the next
	// record must not be glued onto it.
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	return &sink{w: f, close: f.Close}, nil
}

// write sends one record as a single write, so a line is never interleaved or
// half-written by this process.
func (s *sink) write(rec any) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.w.Write(append(data, '\n'))
	return err
}

// tally sums up a run, earlier runs' settled inputs included, into an exit
// code.
type tally map[record.Outcome]int

func (t tally) exitCode() int {
	switch {
	case t[record.NeedsLogin] > 0:
		return exitLogin
	case t[record.Blocked] > 0:
		return exitBlocked
	case t[record.Failed] > 0:
		return exitError
	case t[record.Found] == 0:
		return exitNotFound
	}
	return exitOK
}

func (t tally) String() string {
	keys := make([]string, 0, len(t))
	for k, n := range t {
		if n > 0 {
			keys = append(keys, fmt.Sprintf("%s=%d", k, n))
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

// progress prints a collector's progress events to stderr as one line each.
func (a *app) progress(status string, fields map[string]any) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(status)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, fields[k])
	}
	fmt.Fprintln(a.stderr, b.String())
}

// Command poolclean removes proxy entries that are provably unusable from a
// proxy file.
//
// The gateway already marks a failing proxy unhealthy at runtime, but the file
// it was loaded from is left untouched: every restart retries the same dead
// entries and pays their connect timeouts again before reaching a working one.
// That cost grows with the file, and files assembled from public subscription
// lists routinely carry entries that stopped working weeks ago.
//
// poolclean is the offline counterpart. It dials each entry, and rewrites the
// file without the ones that failed, so the next start begins from a pool that
// is known to route.
//
// The failure rule is deliberately the one the gateway itself uses (see
// isProxyFailure in internal/gateway): only a failure to reach the proxy or to
// tunnel through it counts. Any HTTP response, including 403 and 429, proves the
// route works and keeps the entry. A proxy that reaches opencode.ai and is then
// rate limited is a working proxy; deleting it would discard exactly the
// entries the pool needs most.
//
// Nothing is written unless --write is given, and even then a second pass
// re-tests every candidate on its own before it is dropped.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
)

// defaultTarget is the endpoint every probe requests. It answers 200 quickly
// and needs no credentials, so a probe result reflects the proxy rather than
// the upstream's opinion of the caller.
const defaultTarget = "https://opencode.ai/zen/v1/models"

// verdict values. Only dead entries are ever removed.
const (
	verdictOK      = "ok"
	verdictDead    = "dead"    // the proxy could not be reached or did not tunnel
	verdictSkipped = "skipped" // direct or a rotating pool: never pruned
)

type result struct {
	entry   string
	verdict string
	detail  string
	latency time.Duration
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "poolclean: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "path to config.json; its proxyfile is the file pruned")
	file := flag.String("file", "", "proxy file to prune (default: the proxyfile named by --config)")
	target := flag.String("target", defaultTarget, "URL requested through every proxy")
	workers := flag.Int("workers", 16, "proxies probed concurrently")
	timeout := flag.Duration("timeout", 12*time.Second, "per-proxy budget for one probe")
	rounds := flag.Int("rounds", 2, "a proxy must fail this many consecutive rounds to be a candidate")
	confirm := flag.Int("confirm", 2, "re-tests a candidate on its own; any pass keeps it")
	write := flag.Bool("write", false, "rewrite the proxy file without the dead entries")
	dryRun := flag.Bool("dry-run", false, "report only; the default unless --write is given")
	flag.Parse()

	path := *file
	if path == "" {
		// config.Load validates the whole gateway configuration, which a proxy
		// file cleanup has no business requiring: a file can be pruned before the
		// gateway has keys, and pruning must not fail on an unrelated field.
		proxyFile, err := config.ProxyFileOf(*configPath)
		if err != nil {
			return err
		}
		if proxyFile == "" {
			return errors.New("no proxy file: set --file or proxyfile in the config")
		}
		path = proxyFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(*configPath), path)
		}
	}

	entries, err := readEntries(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Printf("%s: no entries\n", path)
		return nil
	}

	fmt.Printf("%s: %d entries, probing %s\n", path, len(entries), *target)
	results := probeRounds(entries, *target, *workers, *timeout, *rounds)
	report(results)

	candidates := deadEntries(results)
	if len(candidates) == 0 {
		fmt.Println("no entry failed every round; nothing to remove")
		return nil
	}

	kept := confirmCandidates(candidates, *target, *timeout, *confirm)
	drop := make(map[string]bool, len(candidates))
	for _, entry := range candidates {
		if !kept[entry] {
			drop[entry] = true
		}
	}
	if len(drop) == 0 {
		fmt.Printf("%d candidate(s) all passed the confirmation pass; keeping them\n", len(candidates))
		return nil
	}

	names := make([]string, 0, len(drop))
	for entry := range drop {
		names = append(names, entry)
	}
	sort.Strings(names)
	fmt.Printf("%d of %d entries confirmed dead:\n", len(names), len(entries))
	for _, entry := range names {
		fmt.Printf("  %s\n", config.RedactURL(entry))
	}

	if !*write || *dryRun {
		fmt.Println("dry run: rerun with --write to remove them")
		return nil
	}
	if err := rewrite(path, drop); err != nil {
		return err
	}
	fmt.Printf("rewrote %s: %d entries\n", path, len(entries)-len(names))
	return nil
}

// readEntries returns the non-empty, non-comment lines of a proxy file. It
// mirrors the parser the gateway uses for proxyfile so that a line this tool
// reads is a line the gateway would have loaded.
func readEntries(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		line = strings.TrimSpace(stripComment(line))
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries, scanner.Err()
}

// stripComment cuts a line at the first #, ; or // that begins it or follows
// whitespace. A #pool suffix or the // of a URL survives, because neither is
// preceded by a space.
func stripComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

// probeRounds tests every entry rounds times and keeps the last result, so an
// entry is only called dead when it failed each round. A single slow round is
// not evidence: public proxies drop requests under load and recover.
func probeRounds(entries []string, target string, workers int, timeout time.Duration, rounds int) []result {
	if rounds < 1 {
		rounds = 1
	}
	if workers < 1 {
		workers = 1
	}
	last := make([]result, len(entries))
	for round := 0; round < rounds; round++ {
		roundResults := probeAll(entries, target, workers, timeout)
		for i, r := range roundResults {
			// An entry that passed any round stays ok for good.
			if last[i].verdict == verdictOK {
				continue
			}
			if r.verdict == verdictOK || last[i].verdict == "" {
				last[i] = r
			}
		}
	}
	return last
}

func probeAll(entries []string, target string, workers int, timeout time.Duration) []result {
	results := make([]result, len(entries))
	queue := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range queue {
				results[index] = probe(entries[index], target, timeout)
			}
		}()
	}
	for i := range entries {
		queue <- i
	}
	close(queue)
	wg.Wait()
	return results
}

// probe sends one GET through entry. It returns verdictOK for any HTTP
// response, verdictDead only when the route itself failed, and verdictSkipped
// for entries the gateway treats as always usable.
func probe(entry, target string, timeout time.Duration) result {
	clean, isPool := config.ParseProxyEntry(entry)
	if clean == "direct" || isPool {
		return result{entry: entry, verdict: verdictSkipped}
	}

	proxyURL, err := url.Parse(clean)
	if err != nil {
		return result{entry: entry, verdict: verdictDead, detail: "unparsable: " + err.Error()}
	}
	// net/http understands socks5 but not the socks5h spelling; both resolve the
	// destination at the proxy, so the difference is only in the name.
	if proxyURL.Scheme == "socks5h" {
		proxyURL.Scheme = "socks5"
	}

	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout: timeout,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return result{entry: entry, verdict: verdictDead, detail: err.Error()}
	}

	start := time.Now()
	response, err := (&http.Client{Transport: transport}).Do(request)
	latency := time.Since(start)
	if err != nil {
		return result{entry: entry, verdict: verdictDead, detail: describe(err), latency: latency}
	}
	defer response.Body.Close()
	return result{entry: entry, verdict: verdictOK, latency: latency}
}

// describe reduces a transport error to the part that identifies the failure.
func describe(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	text := err.Error()
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return text
}

// confirmCandidates re-tests each candidate alone, with no other probe
// competing for bandwidth, and returns the ones that answered. Batch probing
// occasionally produces a false negative; a candidate is only dropped when it
// also fails on its own.
func confirmCandidates(candidates []string, target string, timeout time.Duration, confirm int) map[string]bool {
	kept := make(map[string]bool, len(candidates))
	if confirm < 1 {
		return kept
	}
	for _, entry := range candidates {
		for attempt := 0; attempt < confirm; attempt++ {
			if probe(entry, target, timeout).verdict == verdictOK {
				kept[entry] = true
				break
			}
		}
	}
	return kept
}

// rewrite writes the proxy file without the dropped entries, keeping every
// other line, including comments, in place. The new file replaces the old one
// atomically so an interrupted run cannot truncate the pool.
func rewrite(path string, drop map[string]bool) error {
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(original)))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		value := strings.TrimSpace(stripComment(strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))))
		if value != "" && drop[value] {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(out.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func deadEntries(results []result) []string {
	var dead []string
	for _, r := range results {
		if r.verdict == verdictDead {
			dead = append(dead, r.entry)
		}
	}
	return dead
}

func report(results []result) {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.verdict]++
	}
	fmt.Printf("ok=%d dead=%d skipped=%d\n", counts[verdictOK], counts[verdictDead], counts[verdictSkipped])
	for _, r := range results {
		if r.verdict == verdictDead {
			fmt.Printf("  dead  %-46s %s\n", config.RedactURL(r.entry), r.detail)
		}
	}
}

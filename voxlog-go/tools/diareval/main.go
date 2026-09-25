// Command diareval measures speaker diarization against a recording whose
// speakers are known.
//
// Every threshold in the pipeline used to be a guess -- sherpa's default, or a
// number that read well in a comment. This is how they stop being guesses: run
// a labelled recording through a sweep of settings and print what each one
// actually did to it.
//
//	go run ./tools/diareval \
//	  -audio ~/Documents/Voxlog/Transcripts/Recordings/2026-09-24-230211-system.wav \
//	  -ref testdata/diarize/2026-09-24-230211.ref
//
// The reference file is one span per line, "start end label", seconds from the
// start of the recording:
//
//	1.4 10.1 A
//	11.9 21.5 B
//
// Lines starting with # are ignored. Labels are arbitrary strings; what is
// measured is whether the diarizer draws the same boundaries, not whether it
// picked the same names for them.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/diarize"
	"voxlog-go/internal/history"
	"voxlog-go/internal/voiceid"
	"voxlog-go/internal/voiceprint"
)

func main() {
	log.SetFlags(0)

	var (
		audioPath  = flag.String("audio", "", "WAV file to diarize (16kHz mono)")
		refPath    = flag.String("ref", "", "reference labels, \"start end label\" per line")
		modelsDir  = flag.String("models", defaultModelsDir(), "directory holding the downloaded models")
		thresholds = flag.String("threshold", "0.4,0.45,0.5,0.55,0.6,0.65,0.7", "clustering thresholds to sweep")
		windows    = flag.String("window", "0,60,600", "diarization window in seconds; 0 is the whole recording")
		merges     = flag.String("merge", "", "link thresholds to sweep, in the centred space; empty means the shipping default")
		clusters   = flag.Int("clusters", -1, "fixed number of speakers, or -1 to decide from the data")
		minOn      = flag.Float64("min-on", float64(diarize.Defaults.MinDurationOn), "shortest reported stretch of speech")
		minOff     = flag.Float64("min-off", float64(diarize.Defaults.MinDurationOff), "shortest silence that ends one")
		timeline   = flag.Bool("timeline", false, "print every segment of every run")
		prints     = flag.Bool("fingerprints", false, "print the similarity between speakers under both fingerprint policies")
		threads    = flag.Int("threads", 4, "model threads")
		embedding  = flag.String("embedding", "", "speaker embedding model to use instead of the one beside the segmentation model")
		compare    = flag.String("compare", "", "comma-separated WAVs to fingerprint whole and compare, instead of diarizing")
		cohort     = flag.String("cohort", "", "directory of WAVs to average into a cohort mean, instead of diarizing")
		exportRef  = flag.String("export-ref", "", "meeting start (RFC3339) whose corrections to write out as a reference")
		store      = flag.String("store", defaultStoreDir(), "directory holding the app's database")
		out        = flag.String("out", "internal/voiceprint/cohort.go", "where -cohort writes its generated source")
	)
	flag.Parse()

	if *exportRef != "" {
		exportReference(*exportRef, *store, *out)
		return
	}
	if *cohort != "" {
		writeCohort(*cohort, *out, *modelsDir, *threads)
		return
	}
	if *compare != "" {
		compareFiles(*compare, *modelsDir, *threads)
		return
	}
	if *audioPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	samples, err := audio.ReadWAV(*audioPath)
	if err != nil {
		log.Fatalf("reading %s: %v", *audioPath, err)
	}
	fmt.Printf("%s: %.1fs\n", filepath.Base(*audioPath), float64(len(samples))/audio.SampleRate)

	var ref []span
	if *refPath != "" {
		if ref, err = readRef(*refPath); err != nil {
			log.Fatalf("reading %s: %v", *refPath, err)
		}
		fmt.Printf("reference: %d spans, %d speakers, %.1fs labelled\n\n", len(ref), countLabels(ref), totalSeconds(ref))
	}

	modelDir := asr.ModelDir(*modelsDir, diarize.Spec)
	if !asr.IsDownloaded(*modelsDir, diarize.Spec) {
		log.Fatalf("diarization models are not in %s -- run the app once with speaker separation on", modelDir)
	}
	embedder, err := voiceid.New(modelDir, *threads)
	if err != nil {
		log.Fatalf("embedding model: %v", err)
	}
	defer embedder.Close()
	fmt.Printf("embedding model: %d dimensions\n\n", embedder.Dim())

	fmt.Printf("%-8s %-10s %-8s %-8s %-8s %-8s %s\n", "window", "cluster", "link", "speakers", "purity", "coverage", "confusion")
	links := []float64{0}
	if *merges != "" {
		links = floats(*merges)
	}
	for _, w := range ints(*windows) {
		for _, t := range floats(*thresholds) {
			for _, l := range links {
				opts := diarize.Options{
					ClusterThreshold: float32(t),
					NumClusters:      *clusters,
					MinDurationOn:    float32(*minOn),
					MinDurationOff:   float32(*minOff),
					LinkThreshold:    float32(l),
					EmbeddingModel:   *embedding,
				}
				d, err := diarize.NewWithOptions(modelDir, *threads, opts)
				if err != nil {
					log.Fatalf("diarizer: %v", err)
				}
				segs := run(d, embedder, *audioPath, samples, w)
				link := d.LinkThreshold()
				d.Close()

				label := fmt.Sprintf("%ds", w)
				if w == 0 {
					label = "whole"
				}
				if len(ref) == 0 {
					fmt.Printf("%-8s %-10.2f %-8.2f %-8d %-8s %-8s %s\n", label, t, link, speakers(segs), "-", "-", "-")
				} else {
					p, c, conf := score(ref, segs)
					fmt.Printf("%-8s %-10.2f %-8.2f %-8d %-8.3f %-8.3f %.3f\n", label, t, link, speakers(segs), p, c, conf)
				}
				if *timeline {
					printTimeline(segs, ref)
				}
				if *prints {
					printFingerprints(embedder, samples, segs)
				}
			}
		}
	}
}

// run diarizes with the window policy under test. 0 hands the whole recording
// to the model at once; 60 is what the app did before this tool existed, one
// ASR block at a time with no stitching across the seams; anything else goes
// through diarize.Windowed, the shipping path.
func run(d *diarize.Diarizer, e *voiceid.Embedder, path string, samples []float32, windowSecs int) []diarize.Segment {
	const maxGap, minDur = 1.5, 0.6
	switch {
	case windowSecs <= 0:
		return diarize.Merge(d.Process(samples), maxGap, minDur)
	case windowSecs == diarize.WindowSeconds:
		// The shipping path exactly: read from the file, a window at a time.
		segs, err := diarize.WindowedFile(d, e, path, maxGap, minDur)
		if err != nil {
			log.Fatalf("diarizing %s: %v", path, err)
		}
		return segs
	default:
		// Independent windows with no stitching: every window renumbers from
		// scratch, which is the failure this whole exercise is about, so it is
		// worth being able to measure it.
		var out []diarize.Segment
		size := windowSecs * audio.SampleRate
		for from, w := 0, 0; from < len(samples); from, w = from+size, w+1 {
			to := min(from+size, len(samples))
			offset := float32(from) / float32(audio.SampleRate)
			for _, s := range diarize.Merge(d.Process(samples[from:to]), maxGap, minDur) {
				s.Start += offset
				s.End += offset
				s.Speaker += w * 100
				out = append(out, s)
			}
		}
		return out
	}
}

// exportReference turns one meeting's corrections into a reference file.
//
// This is how the thresholds in internal/diarize and internal/voiceprint stop
// resting on a single recording labelled by hand in a chat window. Every
// meeting somebody corrects is a labelled recording; this is the door between
// the app and the harness.
//
// It refuses a meeting that is not fully corrected, and says how much is
// missing: a reference with gaps would quietly score the pipeline against its
// own guesses in the parts nobody checked.
func exportReference(id, storeDir, out string) {
	at, err := time.Parse(time.RFC3339, id)
	if err != nil {
		log.Fatalf("meeting id %q: %v (want RFC3339, e.g. 2026-09-24T23:02:11+03:00)", id, err)
	}
	meetings := history.NewMeetingStore(storeDir)

	turns, err := meetings.Turns(at)
	if err != nil {
		log.Fatalf("reading the meeting: %v", err)
	}
	if len(turns) == 0 {
		log.Fatalf("no replies stored for the meeting at %s", id)
	}
	corrections, err := meetings.Corrections(at)
	if err != nil {
		log.Fatalf("reading corrections: %v", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Meeting %s, labelled in the app.\n#\n# start end label\n", at.Format(time.RFC3339))

	var labelled, total float64
	for _, t := range turns {
		if t.Channel != history.ChannelSystem {
			continue
		}
		total += t.EndSecs - t.StartSecs
		name := labelFor(corrections, t)
		if name == "" {
			continue
		}
		labelled += t.EndSecs - t.StartSecs
		fmt.Fprintf(&b, "%.2f %.2f %s\n", t.StartSecs, t.EndSecs, name)
	}

	if total == 0 {
		log.Fatalf("the meeting at %s has no far-end speech to label", id)
	}
	if labelled < total {
		log.Fatalf("only %.1fs of %.1fs is corrected -- correct the rest in the app first, "+
			"or the harness would be scoring the pipeline against its own guesses", labelled, total)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		log.Fatalf("writing %s: %v", out, err)
	}
	fmt.Printf("%s: %.1fs of labelled speech\n", out, labelled)
}

// labelFor names the person a reply was corrected to, by the same midpoint
// rule the store applies corrections by.
func labelFor(corrections []history.Correction, t history.Turn) string {
	mid := (t.StartSecs + t.EndSecs) / 2
	for _, c := range corrections {
		if c.StartSecs <= mid && mid <= c.EndSecs {
			return strings.ReplaceAll(c.Name, " ", "_")
		}
	}
	return ""
}

// defaultStoreDir is where the app keeps its database.
func defaultStoreDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Library", "Application Support", "Voxlog")
}

// writeCohort averages every recording in dir into the mean direction that
// voiceid.Center subtracts, and writes it out as Go source.
//
// Every recording, not a curated set: the mean wanted here is "what everything
// this app records has in common", and the honest sample of that is what the
// app has recorded. Several windows are taken from each file so that a long
// meeting weighs more than a two-second dictation, up to a cap so that one
// very long call cannot become the cohort on its own.
func writeCohort(dir, out, modelsDir string, threads int) {
	const (
		window     = 4 * audio.SampleRate
		perFileCap = 8
	)

	e, err := voiceid.New(asr.ModelDir(modelsDir, diarize.Spec), threads)
	if err != nil {
		log.Fatalf("embedding model: %v", err)
	}
	defer e.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatalf("reading %s: %v", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var (
		sum   []float32
		taken int
		files int
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".wav") {
			continue
		}
		samples, err := audio.ReadWAV(filepath.Join(dir, entry.Name()))
		if err != nil {
			log.Printf("skipping %s: %v", entry.Name(), err)
			continue
		}
		files++
		// Spread the windows over the whole file rather than taking the first
		// few: the opening seconds of a recording are the least typical part
		// of it, and often the quietest.
		windows := min(len(samples)/window, perFileCap)
		for i := 0; i < windows; i++ {
			at := i * (len(samples) - window) / max(windows-1, 1)
			v := e.Compute(samples[at : at+window])
			if len(v) == 0 {
				continue
			}
			if sum == nil {
				sum = make([]float32, len(v))
			}
			if len(v) != len(sum) {
				continue
			}
			for j, x := range v {
				sum[j] += x
			}
			taken++
		}
	}
	if taken == 0 {
		log.Fatalf("no usable audio in %s", dir)
	}
	mean := voiceprint.Normalize(sum)

	var b strings.Builder
	fmt.Fprintf(&b, `package voiceprint

// cohortMean is the average direction of everything this app records: the
// microphone, the system-audio tap, the codecs, the languages actually spoken
// into it. Center subtracts it so that what is left of a fingerprint is the
// speaker.
//
// Generated from %d windows across %d recordings -- do not edit by hand:
//
//	go run ./tools/diareval -cohort <recordings dir> -out %s
//
// Regenerate it when the embedding model changes (a mean from another model is
// the wrong length and Center ignores it, falling back to uncentred cosine),
// or when the app starts recording through a materially different path.
var cohortMean = []float32{
`, taken, files, out)
	for i, x := range mean {
		if i%6 == 0 {
			b.WriteString("\t")
		}
		fmt.Fprintf(&b, "%v,", x)
		if i%6 == 5 || i == len(mean)-1 {
			b.WriteString("\n")
		} else {
			b.WriteString(" ")
		}
	}
	b.WriteString("}\n")

	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		log.Fatalf("writing %s: %v", out, err)
	}
	fmt.Printf("%s: %d dimensions from %d windows across %d recordings\n", out, len(mean), taken, files)
}

// compareFiles is the sanity check behind every threshold here: fingerprint
// whole recordings whose speakers are known from the outside -- two dictations
// by the same person, a call by other people -- and print how similar the
// model says they are. If same and different score alike, no threshold over
// these vectors can work, and the problem is upstream of the clustering.
func compareFiles(list, modelsDir string, threads int) {
	modelDir := asr.ModelDir(modelsDir, diarize.Spec)
	e, err := voiceid.New(modelDir, threads)
	if err != nil {
		log.Fatalf("embedding model: %v", err)
	}
	defer e.Close()

	paths := strings.Split(list, ",")
	raw := make([][]float32, len(paths))
	vecs := make([][]float32, len(paths))
	for i, p := range paths {
		samples, err := audio.ReadWAV(strings.TrimSpace(p))
		if err != nil {
			log.Fatalf("reading %s: %v", p, err)
		}
		// The first eight seconds, the same amount one speaker's fingerprint
		// is built from, so the numbers are comparable with the run above.
		if len(samples) > 8*audio.SampleRate {
			samples = samples[:8*audio.SampleRate]
		}
		raw[i] = e.Compute(samples)
		vecs[i] = voiceprint.Center(raw[i])
		fmt.Printf("%2d  %s\n", i, filepath.Base(strings.TrimSpace(p)))
	}
	printMatrix("raw cosine, the model's own output", raw)
	printMatrix("after the compiled-in cohort mean", vecs)
	printMatrix("after centering on this set alone", centered(raw))
}

// centered subtracts the mean of the set from every vector and renormalises.
//
// Raw cosine between speaker embeddings carries a large component that is not
// the speaker at all -- the microphone, the codec, the room, the language --
// and it is shared by everything recorded the same way, which is why unrelated
// people can score 0.9. Removing the set's own mean removes most of it. It is
// the cheap end of what speaker verification calls score normalisation.
func centered(vecs [][]float32) [][]float32 {
	if len(vecs) == 0 {
		return nil
	}
	mean := voiceprint.WeightedCentroid(vecs, nil)
	out := make([][]float32, len(vecs))
	for i, v := range vecs {
		if len(v) != len(mean) {
			out[i] = v
			continue
		}
		d := make([]float32, len(v))
		for j := range v {
			d[j] = v[j] - mean[j]
		}
		out[i] = voiceprint.Normalize(d)
	}
	return out
}

func printMatrix(title string, vecs [][]float32) {
	printMatrixWith(title, vecs, voiceprint.Cosine)
}

func printMatrixWith(title string, vecs [][]float32, alike func(a, b []float32) float32) {
	fmt.Printf("\n%s\n%4s", title, "")
	for i := range vecs {
		fmt.Printf(" %7d", i)
	}
	fmt.Println()
	for i := range vecs {
		fmt.Printf("%4d", i)
		for j := range vecs {
			fmt.Printf(" %7.3f", alike(vecs[i], vecs[j]))
		}
		fmt.Println()
	}
}

type span struct {
	start, end float64
	label      string
}

func readRef(path string) ([]span, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []span
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < 3 {
			return nil, fmt.Errorf("line %d: want \"start end label\", got %q", line, text)
		}
		start, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", line, err)
		}
		end, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", line, err)
		}
		out = append(out, span{start, end, fields[2]})
	}
	return out, sc.Err()
}

// score reports how well a run matches the reference, in three numbers.
//
// purity is the share of labelled time whose hypothesis speaker is the one
// that speaker's reference label maps to -- the map being the assignment that
// maximises agreement, since speaker numbers are arbitrary. coverage is the
// share of labelled time the diarizer said anything about at all. confusion is
// the share it attributed to the wrong person, which is the number that was
// hiding when three voices came back as one.
func score(ref []span, hyp []diarize.Segment) (purity, coverage, confusion float64) {
	// Seconds of each (reference label, hypothesis speaker) pairing.
	overlap := map[string]map[int]float64{}
	var labelled, covered float64
	for _, r := range ref {
		labelled += r.end - r.start
		if overlap[r.label] == nil {
			overlap[r.label] = map[int]float64{}
		}
		for _, h := range hyp {
			o := min(r.end, float64(h.End)) - max(r.start, float64(h.Start))
			if o <= 0 {
				continue
			}
			overlap[r.label][h.Speaker] += o
			covered += o
		}
	}
	if labelled == 0 {
		return 0, 0, 0
	}

	// Greedy best pairing, largest overlap first: a hypothesis speaker may
	// stand for only one reference label, which is what makes "three voices in
	// one cluster" cost something instead of scoring perfectly.
	type pair struct {
		label   string
		speaker int
		secs    float64
	}
	var pairs []pair
	for label, bySpeaker := range overlap {
		for speaker, secs := range bySpeaker {
			pairs = append(pairs, pair{label, speaker, secs})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].secs > pairs[j].secs })

	usedLabel := map[string]bool{}
	usedSpeaker := map[int]bool{}
	var correct float64
	for _, p := range pairs {
		if usedLabel[p.label] || usedSpeaker[p.speaker] {
			continue
		}
		usedLabel[p.label], usedSpeaker[p.speaker] = true, true
		correct += p.secs
	}
	return correct / labelled, covered / labelled, (covered - correct) / labelled
}

// printFingerprints shows what the meeting-wide linking in voiceid is actually
// deciding on: the similarity between one speaker's fingerprint and the next,
// under the policy that splices a speaker's replies into one buffer and under
// the one that keeps them as separate continuous windows.
//
// voiceprint.MergeThreshold is the line that matters -- anything above it is two
// speakers about to become one -- so it is printed alongside.
func printFingerprints(e *voiceid.Embedder, samples []float32, segs []diarize.Segment) {
	bySpeaker := map[int][]diarize.Segment{}
	var order []int
	for _, s := range segs {
		if _, seen := bySpeaker[s.Speaker]; !seen {
			order = append(order, s.Speaker)
		}
		bySpeaker[s.Speaker] = append(bySpeaker[s.Speaker], s)
	}
	sort.Ints(order)

	for _, policy := range []struct {
		name string
		of   func([]diarize.Segment) []float32
	}{
		{"spliced (before)", func(in []diarize.Segment) []float32 { return spliced(e, samples, in) }},
		{"windowed (after)", func(in []diarize.Segment) []float32 { return diarize.Fingerprint(e, samples, in) }},
	} {
		vecs := make([][]float32, len(order))
		for i, id := range order {
			vecs[i] = policy.of(bySpeaker[id])
		}
		fmt.Printf("  %s, joined across windows above %.2f\n", policy.name, voiceprint.MergeThreshold)
		printMatrix("  raw cosine", vecs)
		printMatrixWith("  as the app compares them", vecs, voiceprint.Similarity)
	}
}

// spliced is how a speaker was fingerprinted before: every slice of their
// speech concatenated into one buffer, capped at eight seconds, handed to the
// model as if it were continuous. Kept here, and only here, so the change can
// be measured rather than asserted.
func spliced(e *voiceid.Embedder, samples []float32, segs []diarize.Segment) []float32 {
	const cap = 8 * audio.SampleRate
	var buf []float32
	for _, s := range segs {
		if len(buf) >= cap {
			break
		}
		buf = append(buf, diarize.Slice(samples, s, audio.SampleRate)...)
	}
	if len(buf) > cap {
		buf = buf[:cap]
	}
	return e.Compute(buf)
}

func printTimeline(segs []diarize.Segment, ref []span) {
	for _, s := range segs {
		fmt.Printf("    %7.2f %7.2f  speaker %-3d  %s\n", s.Start, s.End, s.Speaker, refAt(ref, float64(s.Start), float64(s.End)))
	}
}

// refAt names the reference labels a segment lands on, so a wrong split is
// readable in the timeline instead of only visible in the totals.
func refAt(ref []span, start, end float64) string {
	seen := map[string]bool{}
	var out []string
	for _, r := range ref {
		if min(end, r.end)-max(start, r.start) > 0 && !seen[r.label] {
			seen[r.label] = true
			out = append(out, r.label)
		}
	}
	return strings.Join(out, "+")
}

func speakers(segs []diarize.Segment) int {
	seen := map[int]bool{}
	for _, s := range segs {
		seen[s.Speaker] = true
	}
	return len(seen)
}

func countLabels(ref []span) int {
	seen := map[string]bool{}
	for _, r := range ref {
		seen[r.label] = true
	}
	return len(seen)
}

func totalSeconds(ref []span) float64 {
	var out float64
	for _, r := range ref {
		out += r.end - r.start
	}
	return out
}

func floats(list string) []float64 {
	var out []float64
	for _, f := range strings.Split(list, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil {
			log.Fatalf("bad number %q: %v", f, err)
		}
		out = append(out, v)
	}
	return out
}

func ints(list string) []int {
	var out []int
	for _, f := range strings.Split(list, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			log.Fatalf("bad number %q: %v", f, err)
		}
		out = append(out, v)
	}
	return out
}

// defaultModelsDir is where the app keeps its downloads.
func defaultModelsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "models"
	}
	return filepath.Join(home, "Library", "Application Support", "Voxlog", "models")
}

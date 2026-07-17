package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
)

const version = "1.3.0"

// ── Encoding ──────────────────────────────────────────────────────────────────

func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		raw = raw[2:]
		if len(raw)%2 != 0 {
			raw = append(raw, 0)
		}
		u16 := make([]uint16, len(raw)/2)
		for i := range u16 {
			u16[i] = binary.LittleEndian.Uint16(raw[i*2:])
		}
		return string(utf16.Decode(u16)), nil
	}
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		raw = raw[2:]
		if len(raw)%2 != 0 {
			raw = append(raw, 0)
		}
		u16 := make([]uint16, len(raw)/2)
		for i := range u16 {
			u16[i] = binary.BigEndian.Uint16(raw[i*2:])
		}
		return string(utf16.Decode(u16)), nil
	}
	return string(raw), nil
}

func writeUTF16LE(path, content string) error {
	u16 := utf16.Encode([]rune(content))
	buf := make([]byte, 2+len(u16)*2)
	buf[0], buf[1] = 0xFF, 0xFE
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(buf[2+i*2:], v)
	}
	return os.WriteFile(path, buf, 0644)
}

// ── Data types ────────────────────────────────────────────────────────────────

type Entry struct {
	Original    string `json:"original"`
	Translation string `json:"translation"`
}

// ── Regexes ───────────────────────────────────────────────────────────────────

var (
	reStringTable = regexp.MustCompile(`(?s)STRINGTABLE.*?\{(.*?)\}`)
	// The quoted-string groups below accept doubled quotes ("") as an escaped
	// literal quote character, per RC string-literal syntax — a plain
	// `[^"]+` stops at the first embedded quote and truncates the string.
	reSTEntry   = regexp.MustCompile(`(\d+),\s*"((?:[^"]|"")*)"`)
	reMenuPopup = regexp.MustCompile(`(?:MENUITEM|POPUP)\s+"((?:[^"]|"")*)"`)
	reCaption   = regexp.MustCompile(`CAPTION\s+"((?:[^"]|"")*)"`)
	reControl   = regexp.MustCompile(`CONTROL\s+"((?:[^"]|"")*)",\s*-?\d`)
	// reEscape matches tokens that must survive Google Translate byte-for-byte:
	// classic C-style backslash escapes, printf/MFC-style format placeholders
	// (%s, %1, %1!s!, %%), and an escaped literal ampersand (&&). Order matters
	// within the alternation — more specific patterns are listed before the
	// looser ones they overlap with (e.g. "%1!s!" before plain "%1"), so the
	// longer, more precise token is protected as a single unit.
	reEscape = regexp.MustCompile(`\\x[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}|\\[0-7]{1,3}|\\[ntrb\\'"0]|%\d+![a-zA-Z]+!|%%|%[-+ 0#]*\d*(?:\.\d+)?[sdioxXufFeEgGcp]|%\d+|&&`)
	// reAmpersand finds a Windows accelerator marker. "&&" (escaped literal
	// ampersand) is left alone here — reEscape/protectEscapes protects it in
	// place. A lone "&" marks the next character as the keyboard accelerator
	// and is handled by stripAccelerator/insertAccelerator instead, since that
	// letter essentially never survives translation in the same position.
	reAmpersand = regexp.MustCompile(`&&|&`)
)

func isWinFlag(s string) bool {
	for _, r := range s {
		if !((r >= 'A' && r <= 'Z') || r == '_' || r == '|' || r == ' ') {
			return false
		}
	}
	return len(s) > 0
}

func isServiceString(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r > 127 {
			return false
		}
	}
	return true
}

// ── RC quote escaping ─────────────────────────────────────────────────────────
// RC string literals escape an embedded quote by doubling it:
// `"He said ""hi"" to me"` on disk represents the logical text
// `He said "hi" to me`. Everything else in this program (translation, PO
// export/import, map keys) works with the logical/decoded form; only extract
// (decode) and apply (re-encode) need to know about the doubling.

func rcUnescapeQuotes(s string) string {
	return strings.ReplaceAll(s, `""`, `"`)
}

func rcEscapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, `""`)
}

// ── Escape protection ─────────────────────────────────────────────────────────

func protectEscapes(s string) (string, []string) {
	i := 0
	var result strings.Builder
	var escapes []string
	for {
		loc := reEscape.FindStringIndex(s[i:])
		if loc == nil {
			result.WriteString(s[i:])
			break
		}
		abs := i + loc[0]
		result.WriteString(s[i:abs])
		esc := s[abs : abs+loc[1]-loc[0]]
		// U+E000 is in the Unicode Private Use Area, so it is guaranteed never
		// to occur in real text (unlike, say, "§", which is ordinary text in
		// German legal/business strings — exactly the kind of software this
		// tool localizes).
		result.WriteString(fmt.Sprintf("\uE000ESC%d\uE000", len(escapes)))
		escapes = append(escapes, esc)
		i = abs + loc[1] - loc[0]
	}
	return result.String(), escapes
}

func restoreEscapes(s string, escapes []string) string {
	for i, esc := range escapes {
		s = strings.ReplaceAll(s, fmt.Sprintf("\uE000ESC%d\uE000", i), esc)
	}
	return s
}

// ── Accelerator handling ──────────────────────────────────────────────────────

// stripAccelerator removes a Windows accelerator marker (the lone "&" before
// a menu/control mnemonic, e.g. "&File") before the text goes to machine
// translation. Left in place, it's as likely to be dropped, duplicated, or
// shifted mid-word by the translator as it is to land on a sensible letter in
// the target language. "&&" (escaped literal ampersand) is left untouched —
// see reEscape. The bool return reports whether a marker was found, so the
// caller can reinsert one; see insertAccelerator.
func stripAccelerator(s string) (string, bool) {
	found := false
	cleaned := reAmpersand.ReplaceAllStringFunc(s, func(m string) string {
		if m == "&&" {
			return m
		}
		found = true
		return ""
	})
	return cleaned, found
}

// insertAccelerator prepends "&" just before the first letter or digit of s.
// That's the closest a fully-automated pass can get to a sensible mnemonic;
// translators should still confirm it's unique within its menu/dialog.
func insertAccelerator(s string) string {
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return s[:i] + "&" + s[i:]
		}
	}
	return "&" + s
}

// ── Extract ───────────────────────────────────────────────────────────────────

func extract(content string) map[string]Entry {
	result := make(map[string]Entry)
	for _, block := range reStringTable.FindAllStringSubmatch(content, -1) {
		for _, m := range reSTEntry.FindAllStringSubmatch(block[1], -1) {
			key := "__ST_" + m[1]
			text := rcUnescapeQuotes(m[2])
			result[key] = Entry{Original: text, Translation: text}
		}
	}
	for _, re := range []*regexp.Regexp{reMenuPopup, reCaption, reControl} {
		for _, m := range re.FindAllStringSubmatch(content, -1) {
			text := rcUnescapeQuotes(m[1])
			if strings.TrimSpace(text) == "" || isWinFlag(text) {
				continue
			}
			result[text] = Entry{Original: text, Translation: text}
		}
	}
	return result
}

// ── Google Translate ──────────────────────────────────────────────────────────

// httpClient has an explicit timeout so a stalled connection can't hang a
// worker (and, with enough bad luck, all of them) forever — the default
// http.Client used previously has no deadline at all.
var httpClient = &http.Client{Timeout: 15 * time.Second}

func googleTranslate(text, from, to string) (string, error) {
	cleaned, hadAccel := stripAccelerator(text)
	protected, escapes := protectEscapes(cleaned)
	params := url.Values{}
	params.Set("client", "gtx")
	params.Set("sl", from)
	params.Set("tl", to)
	params.Set("dt", "t")
	params.Set("q", protected)

	resp, err := httpClient.Get("https://translate.googleapis.com/translate_a/single?" + params.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
		return "", fmt.Errorf("google translate: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var raw []interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}
	if len(raw) == 0 {
		return text, nil
	}
	sentences, ok := raw[0].([]interface{})
	if !ok {
		return text, nil
	}
	var sb strings.Builder
	for _, s := range sentences {
		if pair, ok := s.([]interface{}); ok && len(pair) > 0 {
			if word, ok := pair[0].(string); ok {
				sb.WriteString(word)
			}
		}
	}
	translated := sb.String()
	if translated == "" {
		return text, nil
	}
	result := restoreEscapes(translated, escapes)
	if hadAccel {
		result = insertAccelerator(result)
	}
	return result, nil
}

// ── Apply ─────────────────────────────────────────────────────────────────────

type ApplyStats struct {
	Applied  int
	Skipped  int // translation == original (untranslated)
	NotFound int // key not found in RC file
}

func apply(content string, translations map[string]Entry) (string, ApplyStats) {
	var stats ApplyStats

	content = reStringTable.ReplaceAllStringFunc(content, func(block string) string {
		return reSTEntry.ReplaceAllStringFunc(block, func(entry string) string {
			m := reSTEntry.FindStringSubmatch(entry)
			if m == nil {
				return entry
			}
			e, ok := translations["__ST_"+m[1]]
			if !ok {
				return entry
			}
			if e.Translation == e.Original || strings.TrimSpace(e.Translation) == "" {
				stats.Skipped++
				return entry
			}
			stats.Applied++
			return strings.Replace(entry, `"`+m[2]+`"`, `"`+rcEscapeQuotes(e.Translation)+`"`, 1)
		})
	})

	for key, e := range translations {
		if strings.HasPrefix(key, "__ST_") {
			continue
		}
		if e.Translation == e.Original || strings.TrimSpace(e.Translation) == "" {
			stats.Skipped++
			continue
		}
		old := `"` + rcEscapeQuotes(e.Original) + `"`
		new_ := `"` + rcEscapeQuotes(e.Translation) + `"`
		n := 0
		content, n = replaceOutsideStringTable(content, old, new_)
		if n == 0 {
			// Key not found in RC — warn user (possible broken key)
			stats.NotFound++
		} else {
			stats.Applied += n
		}
	}
	return content, stats
}

// replaceOutsideStringTable behaves like strings.Count/strings.ReplaceAll but
// ignores matches inside any STRINGTABLE { ... } block. STRINGTABLE entries
// are already translated above, matched precisely by numeric ID. Short
// strings like "OK", "Cancel", "Yes" are routinely duplicated between a
// STRINGTABLE and a MENUITEM/CONTROL in the same file, and those are tracked
// as separate map entries (STRINGTABLE by ID, everything else by literal
// text) — without this exclusion, a text-based replacement here could leak
// into an unrelated STRINGTABLE entry that was deliberately left
// untranslated, or has its own different translation.
func replaceOutsideStringTable(content, old, new_ string) (string, int) {
	spans := reStringTable.FindAllStringIndex(content, -1)
	if len(spans) == 0 {
		return strings.ReplaceAll(content, old, new_), strings.Count(content, old)
	}
	var sb strings.Builder
	prev := 0
	count := 0
	for _, sp := range spans {
		gap := content[prev:sp[0]]
		count += strings.Count(gap, old)
		sb.WriteString(strings.ReplaceAll(gap, old, new_))
		sb.WriteString(content[sp[0]:sp[1]]) // STRINGTABLE block — left untouched here
		prev = sp[1]
	}
	tail := content[prev:]
	count += strings.Count(tail, old)
	sb.WriteString(strings.ReplaceAll(tail, old, new_))
	return sb.String(), count
}

// ── PO format ─────────────────────────────────────────────────────────────────

func escapePO(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

func unescapePO(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(s[i])
				b.WriteByte(s[i+1])
			}
			i += 2
		} else {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func exportPO(entries map[string]Entry, outPath, srcLang, dstLang string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "# Wolf RPG Editor translation\n# Source: %s  Target: %s\n#\nmsgid \"\"\nmsgstr \"\"\n\"Content-Type: text/plain; charset=UTF-8\\n\"\n\"Content-Transfer-Encoding: 8bit\\n\"\n\"Language: %s\\n\"\n\"MIME-Version: 1.0\\n\"\n\n", srcLang, dstLang, dstLang)
	for key, e := range entries {
		if strings.HasPrefix(key, "__ST_") {
			fmt.Fprintf(w, "msgctxt \"%s\"\n", strings.TrimPrefix(key, "__ST_"))
		}
		fmt.Fprintf(w, "msgid \"%s\"\nmsgstr \"%s\"\n\n", escapePO(e.Original), escapePO(e.Translation))
	}
	return w.Flush()
}

func importPO(path string) (map[string]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]Entry)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var ctx, msgid, msgstr string
	var inMsgstr bool

	flush := func() {
		if msgid == "" {
			ctx, msgid, msgstr, inMsgstr = "", "", "", false
			return
		}
		key := msgid
		if ctx != "" {
			key = "__ST_" + ctx
		}
		result[key] = Entry{Original: msgid, Translation: msgstr}
		ctx, msgid, msgstr, inMsgstr = "", "", "", false
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "msgctxt ") {
			flush()
			ctx = unescapePO(strings.Trim(strings.TrimPrefix(line, "msgctxt "), `"`))
		} else if strings.HasPrefix(line, "msgid ") {
			if msgid != "" {
				flush()
			}
			inMsgstr = false
			msgid = unescapePO(strings.Trim(strings.TrimPrefix(line, "msgid "), `"`))
		} else if strings.HasPrefix(line, "msgstr ") {
			inMsgstr = true
			msgstr = unescapePO(strings.Trim(strings.TrimPrefix(line, "msgstr "), `"`))
		} else if strings.HasPrefix(line, `"`) && inMsgstr {
			msgstr += unescapePO(strings.Trim(line, `"`))
		} else if line == "" && msgid != "" {
			flush()
		}
	}
	flush()
	delete(result, "")
	return result, scanner.Err()
}

// ── Commands ──────────────────────────────────────────────────────────────────

func cmdExtract(rcPath, outPath string) error {
	content, err := readFile(rcPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", rcPath, err)
	}
	entries := extract(content)
	data, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return err
	}
	st := 0
	for k := range entries {
		if strings.HasPrefix(k, "__ST_") {
			st++
		}
	}
	fmt.Printf("Extracted %d strings -> %s\n", len(entries), outPath)
	fmt.Printf("  STRINGTABLE: %d  |  Menus/Dialogs: %d\n", st, len(entries)-st)
	return nil
}

// cmdMerge updates an existing JSON with new strings from an updated RC file.
//   - New strings are added with empty translation
//   - Existing translations are preserved
//   - Strings removed from RC are marked with a ~REMOVED~ prefix so translators
//     know they are obsolete (but their work is not deleted)
func cmdMerge(rcPath, jsonPath string) error {
	content, err := readFile(rcPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", rcPath, err)
	}

	// Load existing translations
	existing := make(map[string]Entry)
	if data, err := os.ReadFile(jsonPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	// Extract fresh set from RC
	fresh := extract(content)

	added, removed, kept := 0, 0, 0

	// Build merged result
	merged := make(map[string]Entry, len(fresh))

	// Add / keep strings that exist in the new RC
	for key, newEntry := range fresh {
		if old, exists := existing[key]; exists {
			// String still exists — keep the translation
			merged[key] = old
			kept++
		} else {
			// Brand new string — no translation yet
			merged[key] = newEntry
			added++
		}
	}

	// Mark strings that disappeared from RC as obsolete
	for key, old := range existing {
		if _, stillExists := fresh[key]; !stillExists {
			// Prefix key so it's clearly obsolete but not lost
			obsoleteKey := "~REMOVED~" + key
			merged[obsoleteKey] = old
			removed++
		}
	}

	data, _ := json.MarshalIndent(merged, "", "  ")
	if err := os.WriteFile(jsonPath, data, 0644); err != nil {
		return err
	}

	fmt.Printf("Merge complete -> %s\n", jsonPath)
	fmt.Printf("  Kept (with translations): %d\n", kept)
	fmt.Printf("  Added (new, untranslated): %d\n", added)
	fmt.Printf("  Removed (marked obsolete): %d\n", removed)
	if removed > 0 {
		fmt.Println("  Tip: obsolete entries are prefixed ~REMOVED~ — safe to delete them.")
	}
	if added > 0 {
		fmt.Println("  Tip: new entries have translation == original — they need translating.")
	}
	return nil
}

func cmdTranslate(jsonPath, outPath, fromLang, toLang string, workers int) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", jsonPath, err)
	}
	var entries map[string]Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	type job struct {
		key  string
		text string
	}
	type result struct {
		key         string
		translation string
	}

	jobs := make(chan job, len(entries))
	results := make(chan result, len(entries))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if isServiceString(j.text) {
					results <- result{key: j.key, translation: j.text}
					continue
				}
				var translated string
				var terr error
				for attempt := 0; attempt < 3; attempt++ {
					translated, terr = googleTranslate(j.text, fromLang, toLang)
					if terr == nil {
						break
					}
					time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				}
				if terr != nil {
					translated = j.text
				}
				results <- result{key: j.key, translation: translated}
			}
		}()
	}

	total := len(entries)
	for key, e := range entries {
		jobs <- job{key: key, text: e.Original}
	}
	close(jobs)
	go func() {
		wg.Wait()
		close(results)
	}()

	translated := make(map[string]Entry, total)
	done := 0
	lastPrint := time.Now()
	for r := range results {
		e := entries[r.key]
		e.Translation = r.translation
		translated[r.key] = e
		done++
		if time.Since(lastPrint) > 2*time.Second || done == total {
			fmt.Printf("\r  Translated: %d / %d (%.0f%%)   ", done, total, float64(done)/float64(total)*100)
			lastPrint = time.Now()
		}
	}
	fmt.Println()

	out, _ := json.MarshalIndent(translated, "", "  ")
	if err := os.WriteFile(outPath, out, 0644); err != nil {
		return err
	}
	fmt.Printf("Done -> %s\n", outPath)
	return nil
}

func cmdApply(rcPath, jsonPath, outPath string) error {
	content, err := readFile(rcPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", rcPath, err)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("cannot read translations: %w", err)
	}
	var translations map[string]Entry
	if err := json.Unmarshal(data, &translations); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	result, stats := apply(content, translations)
	if err := writeUTF16LE(outPath, result); err != nil {
		return fmt.Errorf("cannot write output: %w", err)
	}
	fmt.Printf("Applied %d replacements -> %s (UTF-16-LE)\n", stats.Applied, outPath)
	fmt.Printf("  Untranslated (skipped): %d\n", stats.Skipped)
	if stats.NotFound > 0 {
		fmt.Printf("  WARNING: %d translated strings were not found in RC file.\n", stats.NotFound)
		fmt.Println("  This may mean keys were accidentally edited in the JSON.")
		fmt.Println("  Those translations were NOT applied.")
	}
	return nil
}

func cmdExportPO(jsonPath, outPath, srcLang, dstLang string) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", jsonPath, err)
	}
	var entries map[string]Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := exportPO(entries, outPath, srcLang, dstLang); err != nil {
		return err
	}
	fmt.Printf("Exported %d entries -> %s\n", len(entries), outPath)
	fmt.Println("Open with Poedit, Weblate, or any gettext-compatible editor.")
	return nil
}

func cmdImportPO(poPath, outPath string) error {
	entries, err := importPO(poPath)
	if err != nil {
		return fmt.Errorf("cannot parse .po file: %w", err)
	}
	data, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return err
	}
	fmt.Printf("Imported %d entries -> %s\n", len(entries), outPath)
	return nil
}

// ── Main ──────────────────────────────────────────────────────────────────────

func usage() {
	fmt.Printf(`RC Translator v%s — Wolf RPG Editor localization tool

Commands:
  extract    <input.rc>   <output.json>
             Extract all strings from a fresh .rc file into JSON.
             WARNING: overwrites existing JSON. Use 'merge' for updates.

  merge      <input.rc>   <existing.json>
             Update JSON when .rc file changes:
               - New strings are added (untranslated)
               - Existing translations are preserved
               - Removed strings are marked ~REMOVED~ (not deleted)

  translate  <input.json> <output.json> [--from en] [--to ru] [--workers 10]
             Auto-translate via Google Translate (no API key needed).

  apply      <input.rc>   <translations.json> <output.rc>
             Apply translations and save as UTF-16-LE.
             Reports broken keys (translated strings not found in RC).

  export-po  <input.json> <output.po>  [--from en] [--to ru]
             Export to GNU gettext .po format.
             Compatible with Poedit, Weblate, Crowdin, Transifex.

  import-po  <input.po>   <output.json>
             Import translated .po back to JSON.

  version    Print version.

Workflows:

  First time:
    rc-translator extract   MDS.rc strings.json
    rc-translator translate strings.json strings_ru.json  # optional auto-translate
    rc-translator apply     MDS.rc strings_ru.json MDS_RU.rc

  After RC update:
    rc-translator merge     MDS_new.rc strings_ru.json    # preserves translations!
    rc-translator apply     MDS_new.rc strings_ru.json MDS_RU.rc

  Community translation (Poedit / Weblate):
    rc-translator export-po strings_ru.json strings_ru.po
    ... translate in Poedit or upload to Weblate ...
    rc-translator import-po strings_ru.po strings_ru.json
    rc-translator apply     MDS.rc strings_ru.json MDS_RU.rc

`, version)
}

func parseFlags(args []string) map[string]string {
	flags := map[string]string{"--from": "en", "--to": "ru", "--workers": "10"}
	for i := 0; i < len(args)-1; i++ {
		if _, ok := flags[args[i]]; ok {
			flags[args[i]] = args[i+1]
		}
	}
	return flags
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(0)
	}

	var err error
	switch args[0] {
	case "extract":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: rc-translator extract <input.rc> <output.json>")
			os.Exit(1)
		}
		err = cmdExtract(args[1], args[2])

	case "merge":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: rc-translator merge <input.rc> <existing.json>")
			os.Exit(1)
		}
		err = cmdMerge(args[1], args[2])

	case "translate":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: rc-translator translate <input.json> <output.json> [--from en] [--to ru] [--workers 10]")
			os.Exit(1)
		}
		flags := parseFlags(args[3:])
		workers := 10
		fmt.Sscanf(flags["--workers"], "%d", &workers)
		fmt.Printf("Translating %s -> %s using %d workers\n", flags["--from"], flags["--to"], workers)
		err = cmdTranslate(args[1], args[2], flags["--from"], flags["--to"], workers)

	case "apply":
		if len(args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: rc-translator apply <input.rc> <translations.json> <output.rc>")
			os.Exit(1)
		}
		err = cmdApply(args[1], args[2], args[3])

	case "export-po":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: rc-translator export-po <input.json> <output.po> [--from en] [--to ru]")
			os.Exit(1)
		}
		flags := parseFlags(args[3:])
		err = cmdExportPO(args[1], args[2], flags["--from"], flags["--to"])

	case "import-po":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: rc-translator import-po <input.po> <output.json>")
			os.Exit(1)
		}
		err = cmdImportPO(args[1], args[2])

	case "version":
		fmt.Printf("rc-translator v%s\n", version)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", args[0])
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

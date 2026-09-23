package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
)

const browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"

// runTool implements the subcommands that the built-in skills call through Bash.
func runTool(name string, args []string, out io.Writer) error {
	switch name {
	case "search":
		return cmdSearch(args, out)
	case "fetch":
		return cmdFetch(args, out)
	case "read":
		return cmdRead(args, out)
	}
	return fmt.Errorf("unknown tool %q", name)
}

type searchResult struct {
	Title, URL, Snippet string
}

func cmdSearch(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	count := flags.Int("n", 8, "number of results")
	flags.Usage = func() { fmt.Fprintln(flags.Output(), "usage: search [-n 8] <query>") }
	if err := flags.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" {
		flags.Usage()
		return errors.New("query is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var results []searchResult
	var err error
	if key := os.Getenv("BRAVE_API_KEY"); key != "" {
		results, err = braveSearch(ctx, query, *count, key)
	} else {
		results, err = duckDuckGoSearch(ctx, query, *count)
	}
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(out, "No results.")
	}
	for index, result := range results {
		fmt.Fprintf(out, "%d. %s\n   %s\n", index+1, result.Title, result.URL)
		if result.Snippet != "" {
			fmt.Fprintf(out, "   %s\n", result.Snippet)
		}
	}
	return nil
}

func braveSearch(ctx context.Context, query string, count int, key string) ([]searchResult, error) {
	endpoint := "https://api.search.brave.com/res/v1/web/search?" + url.Values{
		"q": {query}, "count": {strconv.Itoa(min(max(count, 1), 20))},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Subscription-Token", key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave search: HTTP %d", response.StatusCode)
	}
	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.UnmarshalRead(response.Body, &body); err != nil {
		return nil, fmt.Errorf("brave search: %w", err)
	}
	var results []searchResult
	for _, result := range body.Web.Results {
		results = append(results, searchResult{
			Title: stripTags(result.Title), URL: result.URL, Snippet: stripTags(result.Description),
		})
	}
	return results, nil
}

func duckDuckGoSearch(ctx context.Context, query string, count int) ([]searchResult, error) {
	form := url.Values{"q": {query}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", browserUserAgent)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DuckDuckGo returned HTTP %d (likely rate limiting); retry shortly, or set BRAVE_API_KEY in Settings for reliable search", response.StatusCode)
	}
	document, err := html.Parse(io.LimitReader(response.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	var results []searchResult
	challenged := false
	walk(document, func(node *html.Node) bool {
		if node.Type != html.ElementNode {
			return true
		}
		class := attribute(node, "class")
		switch {
		case hasClass(class, "anomaly-modal__modal"):
			challenged = true
			return false
		case hasClass(class, "result--ad"):
			return false
		case node.Data == "a" && hasClass(class, "result__a"):
			results = append(results, searchResult{Title: nodeText(node), URL: duckDuckGoTarget(attribute(node, "href"))})
			return false
		case hasClass(class, "result__snippet") && len(results) > 0:
			results[len(results)-1].Snippet = nodeText(node)
			return false
		}
		return true
	})
	if len(results) == 0 && challenged {
		return nil, errors.New("DuckDuckGo asked for a CAPTCHA (rate limiting); retry shortly, or set BRAVE_API_KEY in Settings for reliable search")
	}
	return results[:min(len(results), max(count, 1))], nil
}

func duckDuckGoTarget(href string) string {
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	if strings.HasSuffix(parsed.Host, "duckduckgo.com") && strings.HasPrefix(parsed.Path, "/l/") {
		if target := parsed.Query().Get("uddg"); target != "" {
			return target
		}
	}
	if parsed.Scheme == "" {
		parsed.Scheme = "https"
	}
	return parsed.String()
}

func cmdFetch(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("fetch", flag.ContinueOnError)
	limit := flags.Int("max", 20000, "maximum characters to print (0 for no limit)")
	offset := flags.Int("offset", 0, "character offset to start from")
	raw := flags.Bool("raw", false, "print the body as-is instead of converting HTML to text")
	flags.Usage = func() { fmt.Fprintln(flags.Output(), "usage: fetch [-max 20000] [-offset 0] [-raw] <url>") }
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("expected one URL")
	}
	target := strings.TrimSpace(flags.Arg(0))
	if !strings.Contains(target, "://") {
		target = "https://" + target
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,text/plain,application/json;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 25<<20))
	if err != nil {
		return err
	}
	final := response.Request.URL
	if response.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d from %s: %s", response.StatusCode, final, paginate(strings.TrimSpace(stripTags(string(body))), 0, 500))
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(body)
		if semicolon := strings.IndexByte(contentType, ';'); semicolon >= 0 {
			contentType = contentType[:semicolon]
		}
	}
	var title, text string
	switch {
	case *raw:
		text = string(body)
	case contentType == "application/pdf":
		temporary, err := os.CreateTemp("", "fetch-*.pdf")
		if err != nil {
			return err
		}
		defer os.Remove(temporary.Name())
		if _, err := temporary.Write(body); err != nil {
			temporary.Close()
			return err
		}
		temporary.Close()
		if text, err = pdfText(temporary.Name()); err != nil {
			return err
		}
	case contentType == "text/html" || contentType == "application/xhtml+xml":
		title, text = htmlToText(body, final)
	case strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") || strings.Contains(contentType, "javascript"):
		text = string(body)
	default:
		return fmt.Errorf("%s is %s (%d bytes), not text; download it with: curl -L -o <file> '%s'", final, contentType, len(body), final)
	}
	fmt.Fprintf(out, "URL: %s\n", final)
	if title != "" {
		fmt.Fprintf(out, "Title: %s\n", title)
	}
	fmt.Fprintf(out, "\n%s\n", paginate(text, *offset, *limit))
	return nil
}

func cmdRead(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("read", flag.ContinueOnError)
	limit := flags.Int("max", 50000, "maximum characters to print (0 for no limit)")
	offset := flags.Int("offset", 0, "character offset to start from")
	flags.Usage = func() { fmt.Fprintln(flags.Output(), "usage: read [-max 50000] [-offset 0] <file>") }
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("expected one file")
	}
	text, err := documentText(flags.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, paginate(text, *offset, *limit))
	return nil
}

// paginate returns at most limit bytes of text starting at offset, on UTF-8
// boundaries, with a note on how to continue.
func paginate(text string, offset, limit int) string {
	total := len(text)
	offset = min(max(offset, 0), total)
	for offset < total && !utf8.RuneStart(text[offset]) {
		offset++
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
		for end > offset && !utf8.RuneStart(text[end]) {
			end--
		}
	}
	page := text[offset:end]
	if end < total {
		page += fmt.Sprintf("\n\n[Truncated: showing characters %d-%d of %d. Continue with -offset %d]", offset, end, total, end)
	}
	return page
}

// documentText extracts readable text from common document formats.
func documentText(file string) (string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", file)
	}
	switch extension := strings.ToLower(filepath.Ext(file)); extension {
	case ".pdf":
		return pdfText(file)
	case ".docx", ".docm", ".dotx":
		return zipXMLText(file, []string{"word/document.xml"}, docxText)
	case ".pptx", ".pptm":
		return pptxText(file)
	case ".xlsx", ".xlsm":
		return xlsxText(file)
	case ".odt", ".ods", ".odp":
		return zipXMLText(file, []string{"content.xml"}, odfText)
	case ".html", ".htm", ".xhtml":
		encoded, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		title, text := htmlToText(encoded, nil)
		if title != "" {
			text = "# " + title + "\n\n" + text
		}
		return text, nil
	case ".doc", ".rtf", ".rtfd", ".wordml", ".webarchive":
		if goruntime.GOOS == "darwin" {
			output, err := exec.Command("textutil", "-convert", "txt", "-stdout", file).Output()
			if err != nil {
				return "", fmt.Errorf("textutil: %w", err)
			}
			return string(output), nil
		}
		return "", fmt.Errorf("no text extractor for %s here; try libreoffice --headless --convert-to txt", extension)
	default:
		if isImage(file) {
			return "", fmt.Errorf("%s is an image; look at it with ViewImage", file)
		}
		encoded, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		sample := encoded[:min(len(encoded), 8192)]
		if bytes.IndexByte(sample, 0) >= 0 || !utf8.Valid(bytes.ToValidUTF8(sample, nil)) {
			return "", fmt.Errorf("%s looks binary (%s); no text extractor for it", file, http.DetectContentType(sample))
		}
		return string(encoded), nil
	}
}

func pdfText(file string) (text string, err error) {
	if pdftotext, lookErr := exec.LookPath("pdftotext"); lookErr == nil {
		if output, runErr := exec.Command(pdftotext, "-layout", "-enc", "UTF-8", file, "-").Output(); runErr == nil && len(bytes.TrimSpace(output)) > 0 {
			return string(output), nil
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("could not parse PDF: %v", recovered)
		}
	}()
	handle, reader, err := pdf.Open(file)
	if err != nil {
		return "", fmt.Errorf("open PDF: %w", err)
	}
	defer handle.Close()
	var builder strings.Builder
	for number := 1; number <= reader.NumPage(); number++ {
		page := reader.Page(number)
		if page.V.IsNull() {
			continue
		}
		content, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		fmt.Fprintf(&builder, "--- Page %d ---\n%s\n\n", number, strings.TrimSpace(content))
	}
	if strings.TrimSpace(strings.ReplaceAll(builder.String(), "--- Page", "")) == "" || !hasLetters(builder.String()) {
		return "", errors.New("no extractable text in this PDF (likely scanned); render pages to images and read them with ViewImage")
	}
	return builder.String(), nil
}

func hasLetters(text string) bool {
	count := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			if count++; count > 20 {
				return true
			}
		}
	}
	return false
}

type xmlTextFunc func(*xml.Decoder, *strings.Builder) error

func zipXMLText(file string, members []string, extract xmlTextFunc) (string, error) {
	archive, err := zip.OpenReader(file)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", file, err)
	}
	defer archive.Close()
	var builder strings.Builder
	for _, member := range members {
		entry, err := archive.Open(member)
		if err != nil {
			return "", fmt.Errorf("%s has no %s: %w", file, member, err)
		}
		err = extract(xml.NewDecoder(entry), &builder)
		entry.Close()
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", member, err)
		}
	}
	return builder.String(), nil
}

// docxText reads WordprocessingML: text in w:t, paragraphs, tabs, and tables.
func docxText(decoder *xml.Decoder, out *strings.Builder) error {
	inText := false
	cells := 0 // depth of open table cells; paragraphs inside cells stay on the row
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "t":
				inText = true
			case "tab":
				out.WriteByte('\t')
			case "br", "cr":
				out.WriteByte('\n')
			case "tc":
				cells++
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				inText = false
			case "p":
				if cells > 0 {
					out.WriteByte(' ')
				} else {
					out.WriteByte('\n')
				}
			case "tc":
				cells--
				out.WriteString("| ")
			case "tr":
				out.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				out.Write(value)
			}
		}
	}
}

// odfText reads OpenDocument content.xml.
func odfText(decoder *xml.Decoder, out *strings.Builder) error {
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "tab":
				out.WriteByte('\t')
			case "line-break":
				out.WriteByte('\n')
			case "s":
				out.WriteByte(' ')
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "p", "h", "table-row":
				out.WriteByte('\n')
			case "table-cell":
				out.WriteByte('\t')
			}
		case xml.CharData:
			out.Write(value)
		}
	}
}

func zipMembers(archive *zip.ReadCloser, prefix, suffix string) []string {
	var names []string
	for _, entry := range archive.File {
		if strings.HasPrefix(entry.Name, prefix) && strings.HasSuffix(entry.Name, suffix) && !strings.Contains(strings.TrimPrefix(entry.Name, prefix), "/") {
			names = append(names, entry.Name)
		}
	}
	// slide10.xml sorts after slide9.xml.
	slices.SortFunc(names, func(a, b string) int {
		return trailingNumber(a, suffix) - trailingNumber(b, suffix)
	})
	return names
}

func trailingNumber(name, suffix string) int {
	stem := strings.TrimSuffix(path.Base(name), suffix)
	start := len(stem)
	for start > 0 && stem[start-1] >= '0' && stem[start-1] <= '9' {
		start--
	}
	number, _ := strconv.Atoi(stem[start:])
	return number
}

func pptxText(file string) (string, error) {
	archive, err := zip.OpenReader(file)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	var out strings.Builder
	for index, member := range zipMembers(archive, "ppt/slides/", ".xml") {
		entry, err := archive.Open(member)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "## Slide %d\n", index+1)
		decoder := xml.NewDecoder(entry)
		inText := false
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			switch value := token.(type) {
			case xml.StartElement:
				inText = inText || value.Name.Local == "t"
			case xml.EndElement:
				switch value.Name.Local {
				case "t":
					inText = false
				case "p":
					out.WriteByte('\n')
				}
			case xml.CharData:
				if inText {
					out.Write(value)
				}
			}
		}
		entry.Close()
		out.WriteByte('\n')
	}
	return out.String(), nil
}

func xlsxText(file string) (string, error) {
	archive, err := zip.OpenReader(file)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	var shared []string
	if entry, err := archive.Open("xl/sharedStrings.xml"); err == nil {
		decoder := xml.NewDecoder(entry)
		var current strings.Builder
		inText := false
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			switch value := token.(type) {
			case xml.StartElement:
				if value.Name.Local == "si" {
					current.Reset()
				}
				inText = inText || value.Name.Local == "t"
			case xml.EndElement:
				switch value.Name.Local {
				case "t":
					inText = false
				case "si":
					shared = append(shared, current.String())
				}
			case xml.CharData:
				if inText {
					current.Write(value)
				}
			}
		}
		entry.Close()
	}
	var names []string
	if entry, err := archive.Open("xl/workbook.xml"); err == nil {
		decoder := xml.NewDecoder(entry)
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			if start, ok := token.(xml.StartElement); ok && start.Name.Local == "sheet" {
				for _, attr := range start.Attr {
					if attr.Name.Local == "name" {
						names = append(names, attr.Value)
					}
				}
			}
		}
		entry.Close()
	}
	var out strings.Builder
	for index, member := range zipMembers(archive, "xl/worksheets/", ".xml") {
		name := fmt.Sprintf("Sheet %d", index+1)
		if index < len(names) {
			name = names[index]
		}
		fmt.Fprintf(&out, "## %s\n", name)
		entry, err := archive.Open(member)
		if err != nil {
			return "", err
		}
		if err := xlsxSheet(xml.NewDecoder(entry), shared, &out); err != nil {
			entry.Close()
			return "", err
		}
		entry.Close()
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// xlsxSheet writes one worksheet as tab-separated rows.
func xlsxSheet(decoder *xml.Decoder, shared []string, out *strings.Builder) error {
	var row []string
	var cellType, cellRef string
	var value strings.Builder
	inValue := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				cellType, cellRef = "", ""
				value.Reset()
				for _, attr := range element.Attr {
					switch attr.Name.Local {
					case "t":
						cellType = attr.Value
					case "r":
						cellRef = attr.Value
					}
				}
			case "v", "t":
				inValue = true
			}
		case xml.EndElement:
			switch element.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				text := value.String()
				if cellType == "s" {
					if index, err := strconv.Atoi(text); err == nil && index >= 0 && index < len(shared) {
						text = shared[index]
					}
				}
				for column := columnIndex(cellRef); len(row) < column; {
					row = append(row, "")
				}
				row = append(row, strings.ReplaceAll(text, "\t", " "))
			case "row":
				out.WriteString(strings.Join(row, "\t"))
				out.WriteByte('\n')
			}
		case xml.CharData:
			if inValue {
				value.Write(element)
			}
		}
	}
}

// columnIndex converts a cell reference like "C7" to a zero-based column.
func columnIndex(ref string) int {
	column := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		column = column*26 + int(r-'A'+1)
	}
	return max(column-1, 0)
}

func walk(node *html.Node, visit func(*html.Node) bool) {
	if !visit(node) {
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func attribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func hasClass(classes, name string) bool {
	return slices.Contains(strings.Fields(classes), name)
}

func nodeText(node *html.Node) string {
	var builder strings.Builder
	walk(node, func(n *html.Node) bool {
		if n.Type == html.TextNode {
			builder.WriteString(n.Data)
		}
		return true
	})
	return strings.Join(strings.Fields(builder.String()), " ")
}

func stripTags(fragment string) string {
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{Type: html.ElementNode, Data: "div"})
	if err != nil {
		return fragment
	}
	var parts []string
	for _, node := range nodes {
		parts = append(parts, nodeText(node))
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func findElement(node *html.Node, tag string) *html.Node {
	var found *html.Node
	walk(node, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if n.Type == html.ElementNode && n.Data == tag {
			found = n
			return false
		}
		return true
	})
	return found
}

func countElements(node *html.Node, tag string) int {
	count := 0
	walk(node, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == tag {
			count++
		}
		return true
	})
	return count
}

// htmlToText renders a page's main content as markdown-flavored text.
func htmlToText(body []byte, base *url.URL) (string, string) {
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", string(body)
	}
	title := ""
	if node := findElement(document, "title"); node != nil {
		title = nodeText(node)
	}
	root := findElement(document, "main")
	if root == nil && countElements(document, "article") == 1 {
		root = findElement(document, "article")
	}
	if root == nil {
		root = findElement(document, "body")
	}
	if root == nil {
		root = document
	}
	writer := &textWriter{base: base, root: root}
	writer.node(root)
	return title, strings.TrimSpace(writer.String())
}

var (
	skippedTags = map[string]bool{
		"script": true, "style": true, "noscript": true, "template": true, "svg": true, "iframe": true,
		"head": true, "button": true, "select": true, "canvas": true, "dialog": true, "form": true,
	}
	chromeTags = map[string]bool{"nav": true, "footer": true, "aside": true}
	blockTags  = map[string]bool{
		"p": true, "div": true, "section": true, "article": true, "main": true, "header": true,
		"ul": true, "ol": true, "table": true, "blockquote": true, "dl": true, "dt": true, "dd": true,
		"figure": true, "figcaption": true, "details": true, "summary": true, "address": true,
	}
)

type textWriter struct {
	buffer  []byte
	base    *url.URL
	root    *html.Node
	pre     int
	spacing bool // whitespace seen since the last written text
}

func (w *textWriter) String() string {
	return string(w.buffer)
}

func (w *textWriter) atLineStart() bool {
	return len(w.buffer) == 0 || w.buffer[len(w.buffer)-1] == '\n'
}

func (w *textWriter) write(text string) {
	if w.spacing && !w.atLineStart() {
		w.buffer = append(w.buffer, ' ')
	}
	w.spacing = false
	w.buffer = append(w.buffer, text...)
}

func (w *textWriter) text(text string) {
	if w.pre > 0 {
		w.buffer = append(w.buffer, text...)
		return
	}
	if text == "" {
		return
	}
	// Whitespace collapses to one space; adjacent text nodes stay joined.
	if first, _ := utf8.DecodeRuneInString(text); unicode.IsSpace(first) {
		w.spacing = true
	}
	for index, field := range strings.FieldsFunc(text, unicode.IsSpace) {
		if index > 0 {
			w.spacing = true
		}
		w.write(field)
	}
	if last, _ := utf8.DecodeLastRuneInString(text); unicode.IsSpace(last) {
		w.spacing = true
	}
}

func (w *textWriter) newline(count int) {
	w.spacing = false
	for len(w.buffer) > 0 && w.buffer[len(w.buffer)-1] == ' ' {
		w.buffer = w.buffer[:len(w.buffer)-1]
	}
	if len(w.buffer) == 0 {
		return
	}
	have := 0
	for index := len(w.buffer) - 1; index >= 0 && w.buffer[index] == '\n'; index-- {
		have++
	}
	for ; have < count; have++ {
		w.buffer = append(w.buffer, '\n')
	}
}

func (w *textWriter) children(node *html.Node) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		w.node(child)
	}
}

func (w *textWriter) node(node *html.Node) {
	switch node.Type {
	case html.TextNode:
		w.text(node.Data)
		return
	case html.DocumentNode:
		w.children(node)
		return
	case html.ElementNode:
	default:
		return
	}
	tag := node.Data
	if skippedTags[tag] || (chromeTags[tag] && node != w.root) {
		return
	}
	if _, hidden := attributeLookup(node, "hidden"); hidden || attribute(node, "aria-hidden") == "true" {
		return
	}
	switch tag {
	case "br":
		w.newline(1)
	case "hr":
		w.newline(2)
		w.write("---")
		w.newline(2)
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.newline(2)
		w.write(strings.Repeat("#", int(tag[1]-'0')) + " ")
		w.children(node)
		w.newline(2)
	case "li":
		w.newline(1)
		w.write("- ")
		w.children(node)
		w.newline(1)
	case "tr":
		w.newline(1)
		w.children(node)
		w.newline(1)
	case "td", "th":
		w.children(node)
		w.write(" |")
		w.spacing = true
	case "pre":
		w.newline(2)
		w.write("```")
		w.newline(1)
		w.pre++
		w.children(node)
		w.pre--
		w.newline(1)
		w.write("```")
		w.newline(2)
	case "a":
		href := w.resolve(attribute(node, "href"))
		if href == "" || nodeText(node) == "" {
			w.children(node)
			return
		}
		w.write("[")
		w.children(node)
		w.spacing = false
		w.buffer = append(w.buffer, "]("+href+")"...)
	case "img":
		if alt := strings.TrimSpace(attribute(node, "alt")); alt != "" {
			w.write("[image: " + alt + "]")
		}
	default:
		if !blockTags[tag] {
			w.children(node)
			return
		}
		gap := 1
		if tag == "p" || tag == "blockquote" || tag == "table" {
			gap = 2
		}
		w.newline(gap)
		w.children(node)
		w.newline(gap)
	}
}

func attributeLookup(node *html.Node, name string) (string, bool) {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val, true
		}
	}
	return "", false
}

func (w *textWriter) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if w.base != nil {
		parsed = w.base.ResolveReference(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "mailto" {
		return ""
	}
	return parsed.String()
}

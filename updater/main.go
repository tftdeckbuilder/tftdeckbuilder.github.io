// TFT 세트 18 덱 빌더 — Windows 실행 파일
// 내장된 덱 빌더 페이지를 로컬 서버(127.0.0.1)로 띄우고 브라우저 앱 창으로 엽니다.
// 페이지 안의 [tftactics.gg에서 자동 업데이트] 버튼은 이 프로그램이 대신 사이트를 읽어 처리합니다.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

const tftacticsURL = "https://tftactics.gg/tierlist/team-comps/"
const opggURL = "https://op.gg/ko/tft/meta-trends/comps"
const labsURL = "https://www.tftlabs.cc/ko"
const lolchessURL = "https://lolchess.gg/meta?hl=ko"
const metatftURL = "https://www.metatft.com/comps"
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

var (
	dataDir  string
	lastPing = time.Now()
	pingMu   sync.Mutex
	logFile  *os.File
)

func logf(format string, a ...any) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, a...) + "\n"
	if logFile != nil {
		logFile.WriteString(line)
	}
	fmt.Print(line)
}

func main() {
	exe, _ := os.Executable()
	dataDir = filepath.Join(filepath.Dir(exe), "data")
	// 명령줄 모드: tftdeck -update all [-data 폴더]  → 서버/브라우저 없이 메타 덱만 갱신하고 종료 (GitHub Actions 등 자동화용)
	if len(os.Args) > 1 {
		os.Exit(runCLI(os.Args[1:]))
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil || !writable(dataDir) {
		dataDir = filepath.Join(os.Getenv("APPDATA"), "TFTDeckBuilder", "data")
		os.MkdirAll(dataDir, 0o755)
	}
	logFile, _ = os.OpenFile(filepath.Join(dataDir, "tftdeck.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logf("포트 열기 실패: %v", err)
		return
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	logf("시작: %s  (데이터 폴더: %s)", url, dataDir)

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/data/", serveData)
	mux.HandleFunc("/__tft/ping", func(w http.ResponseWriter, r *http.Request) {
		pingMu.Lock()
		lastPing = time.Now()
		pingMu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, "tft-host")
	})
	mux.HandleFunc("/__tft/save", handleSave)
	mux.HandleFunc("/__tft/update", handleUpdate)

	go func() { http.Serve(ln, mux) }()
	openBrowser(url)

	// 페이지의 생존 신호(15초 간격)가 90초 이상 끊기면 종료 — 창을 닫으면 프로그램도 꺼집니다.
	for {
		time.Sleep(5 * time.Second)
		pingMu.Lock()
		idle := time.Since(lastPing)
		pingMu.Unlock()
		if idle > 90*time.Second {
			logf("종료 (페이지 닫힘)")
			return
		}
	}
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".w")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexHTML)
}

func serveData(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Path)
	if !dataFiles[name] {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// 페이지의 "이 페이지에 저장": {"data/meta.txt": "...", "data/bis.json": "..."}
func handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var files map[string]string
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&files); err != nil {
		http.Error(w, "잘못된 요청: "+err.Error(), 400)
		return
	}
	for path, content := range files {
		name := filepath.Base(path)
		if !dataFiles[name] {
			continue
		}
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte(content), 0o644); err != nil {
			http.Error(w, "저장 실패: "+err.Error(), 500)
			return
		}
		logf("저장: %s (%d바이트)", name, len(content))
	}
	io.WriteString(w, "ok")
}

// 페이지가 읽고 쓰는 데이터 파일 (data/ 폴더)
var dataFiles = map[string]bool{"meta.txt": true, "meta_metatft.txt": true, "meta_tftlabs.txt": true, "bis.json": true, "emblems.json": true}

// 디버그 덤프: data/debug/ 아래에 저장 (큰 파일은 앞부분만)
const debugMax = 900 << 10

func writeDebug(name string, b []byte) {
	dir := filepath.Join(dataDir, "debug")
	os.MkdirAll(dir, 0o755)
	if len(b) > debugMax {
		b = append(append([]byte{}, b[:debugMax]...), []byte("\n<!-- ... truncated ... -->\n")...)
	}
	os.WriteFile(filepath.Join(dir, name), b, 0o644)
}

type updateResult struct {
	Count  int
	Text   string
	Source string
	File   string
}

// 사이트에서 메타 덱을 받아 파싱하고 data/ 에 저장 (?src=metatft|tftlabs|tftactics)
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	res, err := runUpdate(r.URL.Query().Get("src"))
	if err != nil {
		logf("업데이트 실패: %s", err.Error())
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": res.Count, "text": res.Text, "source": res.Source})
}

func runUpdate(src string) (*updateResult, error) {
	if src != "tftactics" && src != "tftlabs" && src != "opgg" && src != "lolchess" {
		src = "metatft"
	}
	url, file, label := metatftURL, "meta_metatft.txt", "MetaTFT"
	switch src {
	case "tftactics":
		url, file, label = tftacticsURL, "meta.txt", "tftactics.gg"
	case "tftlabs":
		url, file, label = labsURL, "meta_tftlabs.txt", "tftlabs.cc"
	case "opgg":
		url, file, label = opggURL, "meta_metatft.txt", "op.gg"
	case "lolchess":
		url, file, label = lolchessURL, "meta_metatft.txt", "lolchess.gg"
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "ko-KR,ko;q=0.9,en-US;q=0.8")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("사이트 접속 실패: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 30<<20))
	// 디버그용: 받은 원본을 항상 저장 (파서가 이상하면 이 파일을 채팅에 첨부)
	writeDebug(src+"_last.html", body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("사이트 응답 %d", resp.StatusCode)
	}
	var lines string
	var n int
	// JS 렌더링 사이트: 실제 브라우저로 그린 DOM을 먼저 시도
	if src == "metatft" || src == "tftlabs" || src == "lolchess" {
		expand := ""
		if src == "tftlabs" {
			expand = labsExpandJS
		}
		dom, err := renderWithCDP(url, expand)
		if err != nil {
			logf("CDP 렌더링 실패(%s): %v — dump-dom 방식으로 재시도", src, err)
			dom, err = renderDOM(url)
		}
		if err == nil {
			writeDebug(src+"_dom.html", []byte(dom))
			var cs []labsComp
			if src == "tftlabs" {
				cs = parseLabs(dom)
			} else {
				cs = parseRendered(dom)
			}
			var good []labsComp
			for _, c := range cs {
				if len(c.Units) >= 6 {
					good = append(good, c)
				}
			}
			logf("렌더링 파싱(%s): 덱 %d개(유효 %d)", src, len(cs), len(good))
			if len(good) >= 5 {
				source := label + " · " + time.Now().Format("2006-01-02")
				text := "# source: " + source + "\n" + labsText(good)
				if err := os.WriteFile(filepath.Join(dataDir, file), []byte(text), 0o644); err != nil {
					return nil, fmt.Errorf("저장 실패: %v", err)
				}
				return &updateResult{Count: len(good), Text: text, Source: source, File: file}, nil
			}
		} else {
			logf("렌더링 불가(%s): %v — 원본 파싱으로 진행", src, err)
		}
	}
	switch src {
	case "opgg":
		cs := parseOpgg(string(body))
		n = len(cs)
		lines = opggText(cs)
	case "lolchess":
		cs := parseLolchess(string(body))
		n = len(cs)
		lines = labsText(cs)
	case "metatft":
		cs, err := updateMetatft(client, string(body))
		if err != nil {
			return nil, err
		}
		var good []labsComp
		for _, c := range cs {
			if len(c.Units) >= 6 {
				good = append(good, c)
			}
		}
		cs = good
		n = len(cs)
		lines = labsText(cs)
	case "tftlabs":
		cs := parseLabs(string(body))
		n = len(cs)
		lines = labsText(cs)
	default:
		cs := parseComps(string(body))
		n = len(cs)
		lines = compsText(cs)
	}
	if n < 5 {
		writeDebug(src+"_debug.html", body)
		return nil, fmt.Errorf("덱을 %d개만 찾았습니다. 페이지 구조가 바뀐 듯합니다 (data/debug/%s_debug.html 저장됨 — 채팅에 첨부해 주세요)", n, src)
	}
	source := label + " · " + time.Now().Format("2006-01-02")
	text := "# source: " + source + "\n" + lines
	if err := os.WriteFile(filepath.Join(dataDir, file), []byte(text), 0o644); err != nil {
		return nil, fmt.Errorf("저장 실패: %v", err)
	}
	logf("업데이트(%s): 덱 %d개", src, n)
	return &updateResult{Count: n, Text: text, Source: source, File: file}, nil
}

// 명령줄 모드. 사용: tftdeck -update all | -update tftactics,metatft,tftlabs  [-data 폴더]
// 성공한 출처는 data/ 파일을 갱신하고, 하나도 성공하지 못하면 종료 코드 1.
func runCLI(args []string) int {
	srcs := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-update", "--update":
			if i+1 < len(args) {
				srcs = args[i+1]
				i++
			} else {
				srcs = "all"
			}
		case "-data", "--data":
			if i+1 < len(args) {
				dataDir = args[i+1]
				i++
			}
		case "-h", "--help", "-help":
			fmt.Println("사용법: TFT_Set18_DeckBuilder -update all|tftactics,metatft,tftlabs [-data 폴더]")
			return 0
		default:
			fmt.Println("알 수 없는 옵션:", args[i])
			return 2
		}
	}
	if srcs == "" {
		fmt.Println("사용법: TFT_Set18_DeckBuilder -update all|tftactics,metatft,tftlabs [-data 폴더]")
		return 2
	}
	if srcs == "all" {
		srcs = "tftactics,metatft,tftlabs"
	}
	os.MkdirAll(dataDir, 0o755)
	logFile, _ = os.OpenFile(filepath.Join(dataDir, "tftdeck.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	ok := 0
	for _, src := range strings.Split(srcs, ",") {
		src = strings.TrimSpace(src)
		if src == "" {
			continue
		}
		res, err := runUpdate(src)
		if err != nil {
			logf("[%s] 실패: %v (기존 파일 유지)", src, err)
			continue
		}
		logf("[%s] 덱 %d개 → %s", src, res.Count, res.File)
		ok++
	}
	if ok == 0 {
		return 1
	}
	return 0
}

/* ───────── tftactics 파서 (update_meta.py 와 같은 로직) ───────── */

type comp struct {
	Tier, Name string
	Units      []string
}

var (
	reStrip   = regexp.MustCompile(`(?is)<(script|style|noscript|svg)\b.*?</(script|style|noscript|svg)\s*>`)
	reToken   = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*"([^"]*)"[^>]*>(.*?)</a\s*>|<[^>]+>|([^<]+)`)
	reChamp   = regexp.MustCompile(`(?i)/champions?/([a-z0-9\-_]+)/?(?:[?#]|$)`)
	reTag     = regexp.MustCompile(`<[^>]+>`)
	reSpace   = regexp.MustCompile(`\s+`)
	reStrat   = regexp.MustCompile(`(?i)^(fast\s*\d|level\s*\d|slow\s*roll|hyper\s*roll|reroll|re-roll|standard|flex|\d+\s*cost\s*reroll)`)
	reJunk    = regexp.MustCompile(`^[\d★☆•·+\-x×\s]*$`)
	reNumOnly = regexp.MustCompile(`^[\d.%+\-\s#]+$`)
	reStat    = regexp.MustCompile(`(?i)\d+(\.\d+)?%|avg|place|win|rate|top\s*4|play`)
	reLetter  = regexp.MustCompile(`[A-Za-z가-힣]`)
	tiers     = map[string]bool{"S": true, "A": true, "B": true, "C": true, "D": true}
	noise     = map[string]bool{"comps": true, "team comps": true, "tier list": true, "tierlist": true, "meta": true, "champions": true, "items": true, "augments": true, "traits": true, "early game": true, "mid game": true, "late game": true, "best comps": true, "meta comps": true, "new": true, "hot": true, "guide": true, "details": true, "view": true, "more": true, "set 18": true}
)

type token struct{ kind, val string }

func cleanText(s string) string {
	return strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, " ")), " "))
}

func isName(s string) bool {
	s = strings.TrimSpace(s)
	if len([]rune(s)) < 3 || len([]rune(s)) > 40 {
		return false
	}
	if tiers[strings.ToUpper(s)] || reStrat.MatchString(s) || noise[strings.ToLower(s)] || reNumOnly.MatchString(s) || reStat.MatchString(s) {
		return false
	}
	return reLetter.MatchString(s)
}

func parseComps(src string) []comp {
	src = reStrip.ReplaceAllString(src, " ")
	var toks []token
	for _, m := range reToken.FindAllStringSubmatch(src, -1) {
		if m[1] != "" {
			if c := reChamp.FindStringSubmatch(m[1]); c != nil {
				toks = append(toks, token{"champ", strings.ToLower(c[1])})
			} else if t := cleanText(m[2]); t != "" {
				toks = append(toks, token{"text", t})
			}
		} else if m[3] != "" {
			if t := cleanText(m[3]); t != "" {
				toks = append(toks, token{"text", t})
			}
		}
	}
	var comps []comp
	var pending, run []string
	lastTier := ""
	closeRun := func() {
		if len(run) >= 4 {
			tier, name := "", ""
			for i, t := range pending {
				if tiers[strings.ToUpper(t)] {
					tier = strings.ToUpper(t)
					for _, t2 := range pending[i+1:] {
						if isName(t2) {
							name = t2
							break
						}
					}
				}
			}
			if name == "" {
				for _, t := range pending {
					if isName(t) {
						name = t
					}
				}
			}
			if tier != "" {
				lastTier = tier
			}
			if name != "" {
				var units []string
				seen := map[string]bool{}
				for _, s := range run {
					if !seen[s] {
						seen[s] = true
						units = append(units, s)
					}
				}
				tt := lastTier
				if tt == "" {
					tt = "?"
				}
				comps = append(comps, comp{tt, name, units})
			}
		}
		run, pending = nil, nil
	}
	for _, tk := range toks {
		if tk.kind == "champ" {
			run = append(run, tk.val)
			continue
		}
		if len(run) > 0 {
			if !tiers[strings.ToUpper(tk.val)] && (reJunk.MatchString(tk.val) || reStrat.MatchString(tk.val)) {
				continue
			}
			closeRun()
		}
		pending = append(pending, tk.val)
	}
	closeRun()
	// 중복 제거
	seen := map[string]bool{}
	var out []comp
	for _, c := range comps {
		k := c.Name + "|" + strings.Join(c.Units, ",")
		if !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	order := map[string]int{"S": 0, "A": 1, "B": 2, "C": 3, "D": 4}
	sort.SliceStable(out, func(i, j int) bool {
		oi, ok := order[out[i].Tier]
		if !ok {
			oi = 9
		}
		oj, ok := order[out[j].Tier]
		if !ok {
			oj = 9
		}
		return oi < oj
	})
	return out
}

var slugSpecial = map[string]string{"mama-beak": "Mama Beak", "elder-dragon": "Elder Dragon", "the-elder-dragon": "Elder Dragon", "master-yi": "Master Yi", "ancient-sentinel": "Sentinel", "kog-maw": "Kogmaw", "kha-zix": "Khazix", "rek-sai": "RekSai", "le-blanc": "Leblanc"}

func slugName(s string) string {
	if v, ok := slugSpecial[s]; ok {
		return v
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func compsText(cs []comp) string {
	var lines []string
	for _, c := range cs {
		names := make([]string, len(c.Units))
		for i, u := range c.Units {
			names[i] = slugName(u)
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s", c.Tier, c.Name, strings.Join(names, ", ")))
	}
	return strings.Join(lines, "\n")
}

/* ───────── op.gg 파서 (한국어 메타 덱 페이지) ───────── */
// 블록 구조(텍스트 순서): [덱 이름] [게임수] 레벨 N [난이도] 인기 [게임수] 평균 순위 [x] 1등 확률 [y%] 순방 확률 [z%] 픽률 [p%] … [유닛들(1st/2nd/3rd 표시 섞임)]

var koUnits = []string{"라칸", "레오나", "렉사이", "바루스", "베이가", "불타는 묘목", "아칼리", "오른", "요릭", "자야", "조약돌", "카르마", "카밀", "코부코",
	"르블랑", "바위 게", "세주아니", "쉔", "심술두꺼비", "알리스타", "어스름 늑대", "엘리스", "워윅", "유나라", "케이틀린", "케일", "티모",
	"다이애나", "돌거북", "람머스", "렝가", "마스터 이", "바이", "아지르", "어미 부리", "카시오페아", "카직스", "코그모", "트리스타나", "피들스틱", "헤카림",
	"감시자", "니달리", "덩굴정령", "릴리아", "말파이트", "모르가나", "세트", "소라카", "시비르", "아리", "아무무", "아펠리오스", "이즈리얼", "자이라",
	"나르", "드레이븐", "럭스", "마오카이", "아이번", "알룬", "애쉬", "장로 드래곤", "케넨", "타릭"}
var koAlias = map[string]string{"어스름늑대": "어스름 늑대", "바위게": "바위 게", "불타는묘목": "불타는 묘목", "마스터이": "마스터 이", "장로드래곤": "장로 드래곤",
	"어미부리": "어미 부리", "어미 칼날부리": "어미 부리", "어미칼날부리": "어미 부리", "칼날부리": "어미 부리", "고대 파수꾼": "감시자", "고대파수꾼": "감시자"}

type opggComp struct {
	Name, Avg, Top4, Games string
	Units                  []string
}

var (
	reImgAlt   = regexp.MustCompile(`(?is)<img\b[^>]*\balt\s*=\s*"([^"]*)"`)
	reOpToken  = regexp.MustCompile(`(?is)<img\b[^>]*>|<[^>]+>|([^<]+)`)
	reLevel    = regexp.MustCompile(`^레벨\s*\d+$`)
	reNum      = regexp.MustCompile(`^[\d.,%\s]+$`)
	reAvg      = regexp.MustCompile(`평균\s*순위\s*(\d+\.\d{2})`)
	reTop4     = regexp.MustCompile(`순방\s*확률\s*(\d+\.\d{2})%`)
	reGames    = regexp.MustCompile(`인기\s*(\d+)`)
	reLevelAny = regexp.MustCompile(`레벨\s*\d+`)
	reOrdinal  = regexp.MustCompile(`^\d+(st|nd|rd|th)$`)
	opLabels   = map[string]bool{"인기": true, "쉬움": true, "보통": true, "어려움": true, "평균 순위": true, "1등 확률": true, "순방 확률": true, "픽률": true, "난이도": true, "레벨": true}
)

func unitName(s string) string {
	s = strings.TrimSpace(s)
	if a, ok := koAlias[s]; ok {
		return a
	}
	for _, u := range koUnits {
		if s == u {
			return u
		}
	}
	return ""
}

func parseOpgg(src string) []opggComp {
	src = reStrip.ReplaceAllString(src, " ")
	var toks []string
	for _, m := range reOpToken.FindAllStringSubmatch(src, -1) {
		if strings.HasPrefix(strings.ToLower(m[0]), "<img") {
			if a := reImgAlt.FindStringSubmatch(m[0]); a != nil {
				if t := cleanText(a[1]); t != "" {
					toks = append(toks, t)
				}
			}
			continue
		}
		if m[1] != "" {
			if t := cleanText(m[1]); t != "" {
				toks = append(toks, t)
			}
		}
	}
	// 블록 경계: "레벨 N" 토큰
	var starts []int
	for i, t := range toks {
		if reLevel.MatchString(t) || (reLevelAny.MatchString(t) && strings.Contains(t, "평균")) {
			starts = append(starts, i)
		}
	}
	var out []opggComp
	for k, st := range starts {
		end := len(toks)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		// 이름: 레벨 토큰 앞쪽에서 숫자·라벨·유닛명이 아닌 첫 텍스트
		name := ""
		for j := st - 1; j >= 0 && j >= st-6; j-- {
			t := toks[j]
			if reNum.MatchString(t) || opLabels[t] || unitName(t) != "" || reOrdinal.MatchString(t) || reLevel.MatchString(t) {
				continue
			}
			if l := len([]rune(t)); l >= 2 && l <= 30 {
				name = t
			}
			break
		}
		joined := strings.Join(toks[st:end], " ")
		c := opggComp{Name: name}
		if m := reAvg.FindStringSubmatch(joined); m != nil {
			c.Avg = m[1]
		}
		if m := reTop4.FindStringSubmatch(joined); m != nil {
			c.Top4 = m[1]
		}
		if m := reGames.FindStringSubmatch(joined); m != nil {
			c.Games = m[1]
		}
		// 유닛: 픽률 이후 토큰 중 유닛명과 정확히 일치하는 것 (다음 블록 이름 직전까지)
		seen := map[string]bool{}
		after := false
		for j := st; j < end; j++ {
			t := toks[j]
			if !after {
				if strings.Contains(t, "픽률") {
					after = true
				}
				continue
			}
			if u := unitName(t); u != "" && !seen[u] {
				seen[u] = true
				c.Units = append(c.Units, u)
			}
		}
		if len(c.Units) < 4 { // 토큰이 합쳐진 경우: 텍스트에서 유닛명 순서대로 검색
			tail := joined
			if i := strings.Index(tail, "픽률"); i >= 0 {
				tail = tail[i:]
			}
			type hit struct {
				pos int
				u   string
			}
			var hits []hit
			for _, u := range koUnits {
				if p := strings.Index(tail, u); p >= 0 && !seen[u] {
					hits = append(hits, hit{p, u})
				}
			}
			sort.Slice(hits, func(a, b int) bool { return hits[a].pos < hits[b].pos })
			for _, h := range hits {
				if !seen[h.u] {
					seen[h.u] = true
					c.Units = append(c.Units, h.u)
				}
			}
		}
		// 다음 블록의 이름이 유닛 뒤에 붙는 구조라 이름이 비면 앞 블록의 마지막 텍스트를 쓰지 않음
		if len(c.Units) >= 4 && c.Name != "" {
			out = append(out, c)
		}
	}
	return out
}

func opggTier(avg string) string {
	var a float64
	fmt.Sscanf(avg, "%g", &a)
	switch {
	case a == 0:
		return "?"
	case a <= 2.6:
		return "S"
	case a <= 3.1:
		return "A"
	case a <= 3.6:
		return "B"
	case a <= 4.2:
		return "C"
	}
	return "D"
}

func opggText(cs []opggComp) string {
	order := map[string]int{"S": 0, "A": 1, "B": 2, "C": 3, "D": 4}
	sort.SliceStable(cs, func(i, j int) bool { return order[opggTier(cs[i].Avg)] < order[opggTier(cs[j].Avg)] })
	var lines []string
	for _, c := range cs {
		note := ""
		if c.Avg != "" {
			note = " | 평균 " + c.Avg
			if c.Top4 != "" {
				note += " · 순방 " + c.Top4 + "%"
			}
			if c.Games != "" {
				note += " · " + c.Games + "게임"
			}
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s%s", opggTier(c.Avg), c.Name, strings.Join(c.Units, ", "), note))
	}
	return strings.Join(lines, "\n")
}

/* ───────── 렌더링된 DOM 파서 (MetaTFT · lolchess 등 티어 글자 + 유닛 이름 텍스트) ───────── */
// 행 = 티어 글자(S/A/B/C/D) 토큰부터 다음 티어 글자까지. 그 안에서 덱 이름·유닛·통계를 읽는다.
var (
	reTierOnly = regexp.MustCompile(`^(S|A|B|C|D|X)$`)
	reDecimal  = regexp.MustCompile(`^\d\.\d\d$`)
	rePercent  = regexp.MustCompile(`^\d+(\.\d+)?%$`)
	reLevelTok = regexp.MustCompile(`(빠른\s*)?\d+\s*레벨|Fast\s*\d|Level\s*\d`)
	reNoise    = regexp.MustCompile(`(?i)^(HOT|NEW|OP|보통|쉬움|어려움|표준|평균 등수|선택률|승률|순방 확률|Avg\.? Place|Pick|Win|Top ?4|Playrate|Play Rate)$`)
)

func parseRendered(src string) []labsComp {
	stripped := reStrip.ReplaceAllString(src, " ")
	var toks []string
	for _, m := range reOpToken.FindAllStringSubmatch(stripped, -1) {
		if strings.HasPrefix(strings.ToLower(m[0]), "<img") {
			if a := reImgAlt.FindStringSubmatch(m[0]); a != nil {
				if t := cleanText(a[1]); t != "" {
					toks = append(toks, t)
				}
			}
			continue
		}
		if m[1] != "" {
			if t := cleanText(m[1]); t != "" {
				toks = append(toks, t)
			}
		}
	}
	var out []labsComp
	var cur *labsComp
	var pct []string
	var lvl string
	flush := func() {
		if cur != nil && len(cur.Units) >= 4 && cur.Name != "" {
			note := ""
			if cur.Code != "" { // Code 필드를 평균 등수 임시 저장에 사용
				note = "평균 " + cur.Code
			}
			if len(pct) > 0 {
				if note != "" {
					note += " · "
				}
				note += "순방 " + pct[len(pct)-1]
			}
			if lvl != "" {
				if note != "" {
					note += " · "
				}
				note += lvl
			}
			cur.Code = note
			out = append(out, *cur)
		}
		cur = nil
		pct = nil
		lvl = ""
	}
	for _, t := range toks {
		if reTierOnly.MatchString(t) {
			flush()
			tier := t
			if tier == "X" {
				tier = "S"
			}
			cur = &labsComp{Tier: tier}
			continue
		}
		if cur == nil {
			continue
		}
		if u := unitName(t); u != "" {
			dup := false
			for _, x := range cur.Units {
				if x == u {
					dup = true
				}
			}
			if !dup {
				cur.Units = append(cur.Units, u)
			}
			continue
		}
		if reDecimal.MatchString(t) && cur.Code == "" && len(cur.Units) >= 4 {
			cur.Code = t
			continue
		}
		if rePercent.MatchString(t) {
			pct = append(pct, t)
			continue
		}
		if m := reLevelTok.FindString(t); m != "" && lvl == "" {
			lvl = m
			continue
		}
		if cur.Name == "" && len(cur.Units) == 0 && !reNoise.MatchString(t) && !reNum.MatchString(t) && !reOrdinal.MatchString(t) {
			if l := len([]rune(t)); l >= 2 && l <= 40 && reHangul.MatchString(t) {
				cur.Name = t
			}
		}
	}
	flush()
	return out
}

/* ───────── MetaTFT (www.metatft.com/comps) ───────── */
// 페이지는 React 앱이라 HTML엔 데이터가 없다. 흐름:
//  1) /comps HTML에서 JS 번들 주소 수집 → 번들 안에서 metatft API URL(comps 관련) 발굴
//  2) 후보 URL들을 호출해 JSON을 받으면, TFT18_ 유닛 ID 묶음 + 근처 name/tier 필드로 덱 구성
//  3) 응답 원본은 data/metatft_api_last.json 으로 저장 (파서 보정용)

var (
	reScriptSrc = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*"([^"]+\.js[^"]*)"`)
	reApiURL    = regexp.MustCompile(`https?://[a-z0-9.\-]*metatft\.com[^"'\\\s\)]*`)
)

// 번들 발굴이 실패할 때 시도할 알려진 후보들
var metatftGuesses = []string{
	"https://api.metatft.com/tft-comps-api/comps_data?queue=1100&patch=current&days=2&rank=CHALLENGER,GRANDMASTER,MASTER,DIAMOND&permit_filter_adjustment=true",
	"https://api.metatft.com/tft-comps-api/comps_data",
	"https://api2.metatft.com/tft-comps-api/comps_data",
	"https://data.metatft.com/lookups/comps.json",
}

func httpGet(client *http.Client, url string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://www.metatft.com")
	req.Header.Set("Referer", "https://www.metatft.com/comps")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 30<<20))
	return b, resp.StatusCode, nil
}

func updateMetatft(client *http.Client, pageHTML string) ([]labsComp, error) {
	base := "https://www.metatft.com"
	var candidates []string
	seenURL := map[string]bool{}
	add := func(u string) {
		if !seenURL[u] {
			seenURL[u] = true
			candidates = append(candidates, u)
		}
	}
	// 1) 번들에서 API URL 발굴
	var bundles []string
	for _, m := range reScriptSrc.FindAllStringSubmatch(pageHTML, -1) {
		u := m[1]
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		} else if strings.HasPrefix(u, "/") {
			u = base + u
		}
		bundles = append(bundles, u)
	}
	logf("MetaTFT: 번들 %d개", len(bundles))
	for i, bu := range bundles {
		if i >= 4 {
			break
		}
		b, code, err := httpGet(client, bu)
		if err != nil || code != 200 {
			continue
		}
		for _, u := range reApiURL.FindAllString(string(b), -1) {
			lu := strings.ToLower(u)
			if strings.Contains(lu, "comp") && !strings.HasSuffix(lu, ".js") && !strings.HasSuffix(lu, ".css") && !strings.Contains(lu, ".png") {
				add(u)
			}
		}
	}
	for _, g := range metatftGuesses {
		add(g)
	}
	logf("MetaTFT: API 후보 %d개", len(candidates))
	// 2) 후보 호출 → 덱 추출. 모든 후보의 응답 요약을 data/debug/metatft_api.txt 에 남긴다.
	var lastErr string
	var report strings.Builder
	for i, u := range candidates {
		if i >= 14 {
			break
		}
		b, code, err := httpGet(client, u)
		if err != nil {
			lastErr = err.Error()
			fmt.Fprintf(&report, "### %d %s\nERR %s\n\n", i, u, err.Error())
			continue
		}
		head := string(b)
		if len(head) > 1500 {
			head = head[:1500]
		}
		fmt.Fprintf(&report, "### %d %s\nHTTP %d, %d bytes\n%s\n\n", i, u, code, len(b), head)
		if code == 200 && len(b) >= 200 && len(b) < 3<<20 {
			writeDebug(fmt.Sprintf("metatft_api_%d.json", i), b)
		}
		if code != 200 || len(b) < 200 {
			lastErr = fmt.Sprintf("%s → %d", u, code)
			continue
		}
		if strings.Contains(strings.ToLower(u), "unit_items") {
			continue // 유닛-아이템 통계 API: 덱이 아님
		}
		cs := extractCompsFromJSON(string(b))
		named := 0
		for _, c := range cs {
			if !strings.HasPrefix(c.Name, "덱 ") {
				named++
			}
		}
		logf("MetaTFT: %s → %d바이트, 덱 %d(이름 있음 %d)", u, len(b), len(cs), named)
		if len(cs) >= 5 && named*3 >= len(cs) {
			writeDebug("metatft_api_last.json", b)
			writeDebug("metatft_api.txt", []byte(report.String()))
			return cs, nil
		}
	}
	writeDebug("metatft_api.txt", []byte(report.String()))
	return nil, fmt.Errorf("MetaTFT 덱 데이터를 찾지 못했습니다 (%s). data/debug/metatft_api.txt 를 확인해 주세요.", lastErr)
}

// JSON 텍스트에서 TFT18_ 유닛 묶음 + 근처 이름/티어로 덱 추출
func extractCompsFromJSON(body string) []labsComp {
	type hit struct {
		pos int
		ko  string
	}
	var hits []hit
	for _, m := range reApiName.FindAllStringSubmatchIndex(body, -1) {
		nm := strings.ToLower(body[m[2]:m[3]])
		if ko, ok := apiToKo[nm]; ok {
			hits = append(hits, hit{m[0], ko})
		}
	}
	var out []labsComp
	seen := map[string]bool{}
	var cur []string
	curSeen := map[string]bool{}
	start := 0
	last := -1 << 30
	flush := func(endPos int) {
		if len(cur) >= 6 && len(cur) <= 12 {
			key := strings.Join(cur, ",")
			if !seen[key] {
				seen[key] = true
				lo := start - 600
				if lo < 0 {
					lo = 0
				}
				window := body[lo:start]
				name, tier := "", "?"
				for _, m := range reLolName.FindAllStringSubmatch(window, -1) {
					v := jsonUnescape(m[1])
					if l := len([]rune(v)); l >= 3 && l <= 40 && !strings.Contains(v, "TFT") && !strings.Contains(v, "http") {
						name = v
					}
				}
				for _, m := range reLolTier.FindAllStringSubmatch(window, -1) {
					if m[1] != "" {
						tier = lolTierName(m[1])
					} else {
						tier = lolTierName(m[2])
					}
				}
				if name == "" {
					name = "덱 " + fmt.Sprint(len(out)+1)
				}
				out = append(out, labsComp{Tier: tier, Name: name, Units: cur})
			}
		}
		cur = nil
		curSeen = map[string]bool{}
	}
	for _, h := range hits {
		if h.pos-last > 200 || curSeen[h.ko] {
			flush(h.pos)
			start = h.pos
		}
		last = h.pos
		if !curSeen[h.ko] {
			curSeen[h.ko] = true
			cur = append(cur, h.ko)
		}
	}
	flush(len(body))
	return out
}

/* ───────── lolchess.gg 메타 파서 ───────── */
// 화면은 JS로 그려지지만, 원본 응답의 스크립트/JSON(__NEXT_DATA__, self.__next_f 등)에
// 덱 데이터가 들어 있는 경우가 많다. 전략:
//  1) 팀 코드(02…TFTSet18) 문자열을 전부 찾아 해석 → 덱 유닛 확정
//  2) 각 코드 주변에서 덱 이름("name"/"title"/한글 이름)과 티어를 탐색
//  3) 코드가 없으면 TFT18_/DA_ 챔피언 내부 ID 묶음을 덱으로 간주

var (
	reLolName  = regexp.MustCompile(`"(?:name|title|deckName|label)"\s*:\s*"((?:[^"\\]|\\.)+)"`)
	reLolTier  = regexp.MustCompile(`"tier"\s*:\s*(?:"([SABCDOP12345])"|(\d))`)
	reHangul   = regexp.MustCompile(`[가-힣]`)
	reUnescape = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)
	reApiName  = regexp.MustCompile(`(?i)\b(?:TFT18|DA)_(?:18_)?([A-Za-z]+?)(?:18)?(?:_(?:AP|AD|Base|Small|Blossom|Eldritch|Elderwood|Lunar|Coven|Fae|Primal|Inferno|Solar))?\b`)
)

var apiToKo = map[string]string{"ahri": "아리", "akali": "아칼리", "alistar": "알리스타", "alune": "알룬", "amumu": "아무무", "aphelios": "아펠리오스", "ashe": "애쉬", "azir": "아지르",
	"brambleback": "덩굴정령", "caitlyn": "케이틀린", "camille": "카밀", "cassiopeia": "카시오페아", "cinderling": "불타는 묘목", "diana": "다이애나", "draven": "드레이븐",
	"elderdragon": "장로 드래곤", "elise": "엘리스", "ezreal": "이즈리얼", "fiddlesticks": "피들스틱", "gnar": "나르", "gnarsmall": "나르", "gromp": "심술두꺼비",
	"hecarim": "헤카림", "ivern": "아이번", "karma": "카르마", "kayle": "케일", "kennen": "케넨", "khazix": "카직스", "kobuko": "코부코", "kogmaw": "코그모",
	"krug": "돌거북", "leblanc": "르블랑", "leona": "레오나", "lillia": "릴리아", "lux": "럭스", "malphite": "말파이트", "maokai": "마오카이", "masteryi": "마스터 이",
	"morgana": "모르가나", "murkwolf": "어스름 늑대", "nidalee": "니달리", "ornn": "오른", "sentry": "조약돌", "pebbles": "조약돌", "rakan": "라칸", "rammus": "람머스",
	"crimsonraptor": "어미 부리", "mamabeak": "어미 부리", "raptor": "어미 부리", "reksai": "렉사이", "rengar": "렝가", "scuttlecrab": "바위 게", "sejuani": "세주아니", "sentinel": "감시자",
	"sett": "세트", "shen": "쉔", "sivir": "시비르", "soraka": "소라카", "taric": "타릭", "teemo": "티모", "tristana": "트리스타나", "varus": "바루스",
	"veigar": "베이가", "vi": "바이", "warwick": "워윅", "xayah": "자야", "yorick": "요릭", "yunara": "유나라", "zyra": "자이라"}

func jsonUnescape(s string) string {
	s = strings.ReplaceAll(s, `\/`, "/")
	s = reUnescape.ReplaceAllStringFunc(s, func(m string) string {
		var r rune
		fmt.Sscanf(m[2:], "%04x", &r)
		return string(r)
	})
	return s
}

func lolTierName(t string) string {
	switch t {
	case "OP", "S", "1":
		return "S"
	case "A", "2":
		return "A"
	case "B", "3":
		return "B"
	case "C", "4":
		return "C"
	case "D", "5":
		return "D"
	}
	return "?"
}

func parseLolchess(body string) []labsComp {
	var out []labsComp
	seen := map[string]bool{}
	// 전략 1+2: 팀 코드 기준
	for _, loc := range reTeamCode.FindAllStringIndex(body, -1) {
		code := body[loc[0]:loc[1]]
		units := decodeTeamCode(code)
		if len(units) < 4 {
			continue
		}
		key := strings.Join(units, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		// 코드 앞뒤 1200바이트에서 이름·티어 탐색
		lo := loc[0] - 800
		if lo < 0 {
			lo = 0
		}
		window := body[lo:loc[0]] // 이름·티어는 같은 JSON 객체 안, 코드보다 앞에 있다고 가정
		name, tier := "", "?"
		for _, m := range reLolName.FindAllStringSubmatch(window, -1) {
			v := jsonUnescape(m[1])
			if len([]rune(v)) >= 2 && len([]rune(v)) <= 30 && reHangul.MatchString(v) {
				name = v // 가장 가까운(마지막) 것
			}
		}
		for _, m := range reLolTier.FindAllStringSubmatch(window, -1) {
			if m[1] != "" {
				tier = lolTierName(m[1])
			} else {
				tier = lolTierName(m[2])
			}
		}
		if name == "" {
			name = fmt.Sprintf("덱 %d", len(out)+1)
		}
		out = append(out, labsComp{Tier: tier, Name: name, Units: units, Code: code})
	}
	if len(out) >= 5 {
		return out
	}
	// 전략 3: 챔피언 내부 ID 묶음 (TFT18_Xxx / DA_Xxx18) — 400바이트 안에 연달아 나오는 묶음을 덱으로
	type hit struct {
		pos int
		ko  string
	}
	var hits []hit
	for _, m := range reApiName.FindAllStringSubmatchIndex(body, -1) {
		nm := strings.ToLower(body[m[2]:m[3]])
		if ko, ok := apiToKo[nm]; ok {
			hits = append(hits, hit{m[0], ko})
		}
	}
	var cur []string
	curSeen := map[string]bool{}
	last := -1 << 30
	flush := func() {
		if len(cur) >= 6 && len(cur) <= 12 {
			key := strings.Join(cur, ",")
			if !seen[key] {
				seen[key] = true
				out = append(out, labsComp{Tier: "?", Name: fmt.Sprintf("덱 %d", len(out)+1), Units: cur})
			}
		}
		cur = nil
		curSeen = map[string]bool{}
	}
	for _, h := range hits {
		if h.pos-last > 150 || curSeen[h.ko] {
			flush()
		}
		last = h.pos
		if !curSeen[h.ko] {
			curSeen[h.ko] = true
			cur = append(cur, h.ko)
		}
	}
	flush()
	return out
}

/* ───────── tftlabs.cc 파서 ───────── */
// 목록 행: [덱 이름(+HOT/NEW)] [특성 아이콘] [팀 코드 복사] [유닛들] [코스트 합] [공략 더 보기]
// 티어는 섹션 제목("S 티어" 또는 "S")으로 나옴. 팀 코드 문자열(02…TFTSet18)이 HTML에 있으면 그걸 우선 사용.

type labsComp struct {
	Tier, Name string
	Units      []string
	Code       string
}

var (
	reLabsTier = regexp.MustCompile(`^([SABCD])(\s*티어)?$`)
	reTeamCode = regexp.MustCompile(`0[12][0-9a-fA-F]{18,60}TFTSet\d+`)
	labsSkip   = map[string]bool{"HOT": true, "NEW": true, "팀 코드 복사": true, "공략 더 보기": true, "OP": true}
	// 팀 코드 → 한글 이름 (덱 코드 해석용)
	codeToKo = map[int]string{1026: "심술두꺼비", 1049: "어스름 늑대", 1065: "조약돌", 1015: "불타는 묘목", 1062: "바위 게", 1039: "돌거북", 1064: "감시자", 1011: "덩굴정령",
		1081: "자야", 1055: "오른", 1051: "니달리", 1027: "헤카림", 1023: "이즈리얼", 1009: "아지르", 1059: "렉사이", 1078: "베이가", 1045: "마오카이", 1044: "말파이트",
		1082: "요릭", 1083: "유나라", 1066: "세트", 1001: "아리", 1008: "애쉬", 1021: "엘리스", 1012: "케이틀린", 1014: "카시오페아", 1048: "모르가나", 1056: "라칸",
		1042: "릴리아", 1085: "코부코", 1073: "티모", 1057: "람머스", 1025: "나르", 1075: "트리스타나", 1068: "시비르", 1079: "바이", 1002: "아칼리", 1077: "바루스",
		1005: "아무무", 1035: "케넨", 1007: "아펠리오스", 1018: "다이애나", 1004: "알룬", 1041: "레오나", 1034: "케일", 1063: "세주아니", 1036: "카직스", 1060: "렝가",
		1038: "코그모", 1084: "자이라", 1024: "피들스틱", 1070: "소라카", 1072: "타릭", 1029: "아이번", 1043: "럭스", 1003: "알리스타", 1019: "드레이븐", 1020: "장로 드래곤",
		1031: "카르마", 1040: "르블랑", 1046: "마스터 이", 1067: "쉔", 1080: "워윅", 1013: "카밀", 1058: "어미 부리"}
)

func decodeTeamCode(code string) []string {
	m := regexp.MustCompile(`^0([12])([0-9a-fA-F]+)TFTSet\d+$`).FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	w := 3
	if m[1] == "1" {
		return nil // 구형식은 세트 내 인덱스라 해석하지 않음
	}
	hex := m[2]
	var out []string
	seen := map[string]bool{}
	for i := 0; i+w <= len(hex); i += w {
		var v int
		fmt.Sscanf(hex[i:i+w], "%x", &v)
		if v == 0 {
			continue
		}
		if n, ok := codeToKo[v]; ok && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func parseLabs(src string) []labsComp {
	// 팀 코드가 속성에 있을 수 있으니 태그 제거 전에 전체에서 코드 수집 위치 기록
	rawCodes := reTeamCode.FindAllStringIndex(src, -1)
	stripped := reStrip.ReplaceAllString(src, " ")
	var toks []string
	for _, m := range reOpToken.FindAllStringSubmatchIndex(stripped, -1) {
		frag := stripped[m[0]:m[1]]
		if strings.HasPrefix(strings.ToLower(frag), "<img") {
			if a := reImgAlt.FindStringSubmatch(frag); a != nil {
				if t := cleanText(a[1]); t != "" {
					toks = append(toks, t)
				}
			}
			continue
		}
		if m[2] >= 0 {
			if t := cleanText(stripped[m[2]:m[3]]); t != "" {
				toks = append(toks, t)
			}
		}
	}
	tier := "?"
	var out []labsComp
	cur := labsComp{}
	flush := func() {
		if cur.Name != "" && (len(cur.Units) > 0 || cur.Code != "") {
			out = append(out, cur)
		}
		cur = labsComp{}
	}
	for _, t := range toks {
		if m := reLabsTier.FindStringSubmatch(t); m != nil {
			tier = m[1]
			continue
		}
		if t == "공략 더 보기" { // 행의 끝
			cur.Tier = tier
			flush()
			continue
		}
		if t == "__DETAIL__" { // 클릭으로 펼친 상세가 이어짐: 상세의 유닛 순서를 우선
			cur.Units = nil
			continue
		}
		if labsSkip[t] || reNum.MatchString(t) || reOrdinal.MatchString(t) {
			continue
		}
		if c := reTeamCode.FindString(t); c != "" {
			cur.Code = c
			continue
		}
		if u := unitName(t); u != "" {
			dup := false
			for _, x := range cur.Units {
				if x == u {
					dup = true
				}
			}
			if !dup {
				cur.Units = append(cur.Units, u)
			}
			continue
		}
		if cur.Name == "" && len([]rune(t)) >= 2 && len([]rune(t)) <= 30 && !strings.Contains(t, "상징") {
			cur.Name = t
			cur.Tier = tier
		}
	}
	flush()
	// 텍스트 토큰에 코드가 없으면 원본 속성에서 찾은 코드를 행 순서대로 배정
	if len(rawCodes) >= len(out) && len(out) > 0 {
		hasAny := false
		for _, c := range out {
			if c.Code != "" {
				hasAny = true
			}
		}
		if !hasAny {
			for i := range out {
				if i < len(rawCodes) {
					out[i].Code = src[rawCodes[i][0]:rawCodes[i][1]]
				}
			}
		}
	}
	// 코드가 있으면 유닛을 코드 기준으로 교체(더 정확)
	for i := range out {
		if us := decodeTeamCode(out[i].Code); len(us) >= 4 {
			out[i].Units = us
		}
	}
	return out
}

func labsText(cs []labsComp) string {
	order := map[string]int{"S": 0, "A": 1, "B": 2, "C": 3, "D": 4}
	sort.SliceStable(cs, func(i, j int) bool {
		oi, ok := order[cs[i].Tier]
		if !ok {
			oi = 9
		}
		oj, ok := order[cs[j].Tier]
		if !ok {
			oj = 9
		}
		return oi < oj
	})
	var lines []string
	for _, c := range cs {
		note := ""
		if c.Code != "" {
			if reTeamCode.MatchString(c.Code) {
				note = " | 코드 " + c.Code
			} else {
				note = " | " + c.Code
			}
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s%s", c.Tier, c.Name, strings.Join(c.Units, ", "), note))
	}
	return strings.Join(lines, "\n")
}

/* ───────── 브라우저 열기: Edge/Chrome 앱 창 → 기본 브라우저 ───────── */

func findBrowser() string {
	if b := os.Getenv("TFT_BROWSER"); b != "" {
		return b
	}
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	local := os.Getenv("LOCALAPPDATA")
	cands := []string{
		filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
		"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser", "/opt/pw-browsers/chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func openBrowser(url string) {
	if c := findBrowser(); c != "" {
		cmd := exec.Command(c, "--app="+url, "--window-size=1500,1000", "--user-data-dir="+filepath.Join(dataDir, "browser-profile"))
		if err := cmd.Start(); err == nil {
			logf("브라우저 앱 창: %s", c)
			return
		}
	}
	logf("기본 브라우저로 엽니다")
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// 화면 없는 브라우저로 페이지를 완전히 렌더링한 뒤 DOM(HTML)을 받아온다.
// JS로 그려지는 사이트(MetaTFT, lolchess, tftlabs)도 사람이 보는 그대로의 텍스트를 얻을 수 있다.
func renderDOM(url string) (string, error) {
	b := findBrowser()
	if b == "" {
		return "", fmt.Errorf("Edge/Chrome을 찾지 못했습니다")
	}
	profile := filepath.Join(os.TempDir(), "tftdeck-headless")
	args := []string{"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--hide-scrollbars", "--window-size=1400,4000", "--lang=ko-KR", "--accept-lang=ko-KR,ko",
		"--user-agent=" + userAgent, "--virtual-time-budget=25000", "--user-data-dir=" + profile, "--dump-dom", url}
	cmd := exec.Command(b, args...)
	out, err := cmd.Output()
	if err != nil && len(out) < 1000 {
		return "", fmt.Errorf("브라우저 렌더링 실패: %v", err)
	}
	if len(out) < 1000 {
		return "", fmt.Errorf("렌더링 결과가 비어 있습니다")
	}
	return string(out), nil
}

package main

// 최소 DevTools 프로토콜(CDP) 클라이언트 — 외부 라이브러리 없이 표준 라이브러리만 사용.
// headless Edge/Chrome을 원격 디버깅 포트로 띄우고, 페이지 안에서 JS를 실행(클릭·대기)한 뒤 DOM을 받아온다.

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

/* ───────── WebSocket (클라이언트, RFC 6455 최소 구현) ───────── */

type wsConn struct {
	c  net.Conn
	br *bufio.Reader
}

func wsDial(wsURL string, timeout time.Duration) (*wsConn, error) {
	u := strings.TrimPrefix(wsURL, "ws://")
	host := u
	path := "/"
	if i := strings.Index(u, "/"); i >= 0 {
		host, path = u[:i], u[i:]
	}
	c, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, host, k)
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(c, 1<<20)
	c.SetReadDeadline(time.Now().Add(timeout))
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		c.Close()
		return nil, fmt.Errorf("websocket 업그레이드 실패: %s %v", strings.TrimSpace(status), err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			c.Close()
			return nil, err
		}
		if line == "\r\n" {
			break
		}
	}
	c.SetReadDeadline(time.Time{})
	return &wsConn{c: c, br: br}, nil
}

func (w *wsConn) writeText(payload []byte) error {
	var hdr []byte
	n := len(payload)
	hdr = append(hdr, 0x81) // FIN + text
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 65536:
		hdr = append(hdr, 0x80|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.c.Write(hdr); err != nil {
		return err
	}
	_, err := w.c.Write(masked)
	return err
}

// 한 메시지(조각 포함) 읽기. ping엔 pong으로 응답.
func (w *wsConn) readMessage(timeout time.Duration) ([]byte, error) {
	w.c.SetReadDeadline(time.Now().Add(timeout))
	defer w.c.SetReadDeadline(time.Time{})
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.br, h); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0f
		n := uint64(h[1] & 0x7f)
		masked := h[1]&0x80 != 0
		switch n {
		case 126:
			b := make([]byte, 2)
			io.ReadFull(w.br, b)
			n = uint64(binary.BigEndian.Uint16(b))
		case 127:
			b := make([]byte, 8)
			io.ReadFull(w.br, b)
			n = binary.BigEndian.Uint64(b)
		}
		var mk []byte
		if masked {
			mk = make([]byte, 4)
			io.ReadFull(w.br, mk)
		}
		if n > 200<<20 {
			return nil, fmt.Errorf("메시지가 너무 큽니다")
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(w.br, data); err != nil {
			return nil, err
		}
		if masked {
			for i := range data {
				data[i] ^= mk[i%4]
			}
		}
		switch op {
		case 0x9: // ping → pong
			pong := append([]byte{0x8a, 0x80 | byte(len(data))}, []byte{0, 0, 0, 0}...)
			w.c.Write(append(pong, data...))
			continue
		case 0x8:
			return nil, fmt.Errorf("websocket 종료")
		case 0xA:
			continue
		}
		msg = append(msg, data...)
		if fin {
			return msg, nil
		}
	}
}

/* ───────── CDP 세션 ───────── */

type cdpSession struct {
	ws  *wsConn
	id  int
	cmd *exec.Cmd
}

func (s *cdpSession) call(method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	s.id++
	id := s.id
	body, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := s.ws.writeText(body); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := s.ws.readMessage(time.Until(deadline))
		if err != nil {
			return nil, err
		}
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &m) != nil || m.ID != id {
			continue // 이벤트 등은 무시
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		return m.Result, nil
	}
	return nil, fmt.Errorf("%s: 응답 시간 초과", method)
}

// 페이지 안에서 JS를 실행하고 문자열 결과를 돌려받는다 (Promise 지원).
func (s *cdpSession) eval(js string, timeout time.Duration) (string, error) {
	res, err := s.call("Runtime.evaluate", map[string]any{"expression": js, "awaitPromise": true, "returnByValue": true}, timeout)
	if err != nil {
		return "", err
	}
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	json.Unmarshal(res, &r)
	if r.ExceptionDetails != nil {
		return "", fmt.Errorf("페이지 스크립트 오류: %s", r.ExceptionDetails.Text)
	}
	if v, ok := r.Result.Value.(string); ok {
		return v, nil
	}
	b, _ := json.Marshal(r.Result.Value)
	return string(b), nil
}

func (s *cdpSession) close() {
	if s.ws != nil {
		s.ws.c.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
}

// headless 브라우저를 띄우고 URL을 연 페이지 세션을 돌려준다.
func openCDP(url string) (*cdpSession, error) {
	b := findBrowser()
	if b == "" {
		return nil, fmt.Errorf("Edge/Chrome을 찾지 못했습니다")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	profile := filepath.Join(os.TempDir(), "tftdeck-cdp")
	args := []string{"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--hide-scrollbars", "--window-size=1400,4000", "--lang=ko-KR", "--accept-lang=ko-KR,ko", "--user-agent=" + userAgent,
		fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir=" + profile, url}
	cmd := exec.Command(b, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sess := &cdpSession{cmd: cmd}
	// 디버깅 포트가 열릴 때까지 대기 후 페이지 타깃의 ws 주소 조회
	var wsURL string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && wsURL == "" {
		time.Sleep(300 * time.Millisecond)
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
		if err != nil {
			continue
		}
		var targets []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
			WS   string `json:"webSocketDebuggerUrl"`
		}
		json.NewDecoder(resp.Body).Decode(&targets)
		resp.Body.Close()
		for _, t := range targets {
			if t.Type == "page" && t.WS != "" {
				wsURL = t.WS
				break
			}
		}
	}
	if wsURL == "" {
		sess.close()
		return nil, fmt.Errorf("브라우저 디버깅 포트 연결 실패")
	}
	ws, err := wsDial(wsURL, 10*time.Second)
	if err != nil {
		sess.close()
		return nil, err
	}
	sess.ws = ws
	// 페이지 로드 대기: readyState complete + 추가 안정화
	// 내비게이션 중이면 실행 컨텍스트가 파괴될 수 있으므로 성공할 때까지 재시도
	waitJS := `new Promise(r=>{ const t0=Date.now(); const chk=()=>{ if(document.readyState==='complete' && Date.now()-t0>2500) r('ok'); else if(Date.now()-t0>20000) r('timeout'); else setTimeout(chk,200); }; chk(); })`
	var lastErr error
	for i := 0; i < 15; i++ {
		if _, err := sess.eval(waitJS, 25*time.Second); err == nil {
			return sess, nil
		} else {
			lastErr = err
			if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "Cannot find") {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	sess.close()
	return nil, lastErr
}

// 페이지를 렌더링하고, expandJS(있으면)를 실행해 클릭 등으로 내용을 펼친 뒤 최종 DOM을 돌려준다.
func renderWithCDP(url, expandJS string) (string, error) {
	sess, err := openCDP(url)
	if err != nil {
		return "", err
	}
	defer sess.close()
	// 스크롤로 지연 로딩되는 목록도 끝까지 그리도록 아래로 여러 번 스크롤
	scrollJS := `new Promise(async r=>{ const s=ms=>new Promise(x=>setTimeout(x,ms)); for(let i=0;i<12;i++){ window.scrollTo(0, document.body.scrollHeight); await s(350); } window.scrollTo(0,0); await s(400); r('ok'); })`
	sess.eval(scrollJS, 20*time.Second)
	if expandJS != "" {
		if out, err := sess.eval(expandJS, 90*time.Second); err != nil {
			logf("펼치기 스크립트 오류: %v", err)
		} else {
			logf("펼치기: %s", firstN(out, 120))
		}
	}
	return sess.eval(`document.documentElement.outerHTML`, 30*time.Second)
}

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// tftlabs: 각 덱 행(팀 코드 복사/공략 더 보기가 있는 행)의 첫 아이콘/이름을 클릭해 상세를 펼치고,
// 펼쳐진 내용을 행 안에 <div data-tft-detail> 로 복사해 둔다(닫히더라도 DOM에 남도록).
const labsExpandJS = `new Promise(async resolve=>{
  const sleep=ms=>new Promise(r=>setTimeout(r,ms));
  const leafOf=re=>[...document.querySelectorAll('body *')].filter(e=>e.childElementCount===0 && !/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE)$/.test(e.tagName) && re.test(e.textContent||''));
  const rows=[]; const seen=new Set();
  for(const m of leafOf(/팀 코드 복사|공략 더 보기/)){ let r=m; for(let i=0;i<10&&r.parentElement;i++){ r=r.parentElement; if(r.querySelectorAll('img').length>=5) break; } if(!seen.has(r)){ seen.add(r); rows.push(r); } }
  const initial=new Set([document.documentElement, document.body, ...document.querySelectorAll('body *')]);
  let done=0;
  for(const r of rows){
    const before=new Set([document.documentElement, document.body, ...document.querySelectorAll('body *')]);
    const target=r.querySelector('img')||r.querySelector('h3,h2,strong,b,span')||r;
    try{ target.scrollIntoView({block:'center'}); target.click(); }catch(e){}
    await sleep(800);
    // 클릭 후 새로 생긴 요소들 중 유닛 아이콘이 5개 이상인 최상위 요소 = 상세 내용
    const fresh=[...document.querySelectorAll('body *')].filter(e=>!before.has(e));
    const roots=fresh.filter(e=>!(e.parentElement && !before.has(e.parentElement)) && e.querySelectorAll('img').length>=5);
    let detail=roots.sort((a,b)=>b.innerText.length-a.innerText.length)[0]||null;
    if(!detail){ // 행 자체가 제자리에서 펼쳐진 경우: 행 안의 새 요소
      const inRow=fresh.filter(e=>r.contains(e) && !(e.parentElement && !before.has(e.parentElement)));
      if(inRow.length) { detail=document.createElement('div'); inRow.forEach(e=>detail.appendChild(e.cloneNode(true))); }
    }
    // '공략 더 보기' 표시를 행 맨 끝으로 옮기고 그 앞에 상세를 끼워 넣는다(파서가 한 행으로 읽도록)
    const more=[...r.querySelectorAll('*')].find(e=>e.childElementCount===0 && /공략 더 보기/.test(e.textContent||''));
    if(more) more.textContent='';
    if(detail){ const d=document.createElement('div'); d.setAttribute('data-tft-detail','1'); d.innerHTML='<span>__DETAIL__</span>'+detail.innerHTML; d.querySelectorAll('*').forEach(e=>{ if(e.childElementCount===0 && /팀 코드 복사|공략 더 보기/.test(e.textContent||'')) e.textContent=''; }); r.appendChild(d); done++; }
    const end=document.createElement('span'); end.textContent='공략 더 보기'; r.appendChild(end);
    try{ document.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape'})); const close=[...document.querySelectorAll('button')].find(b=>/닫기|close|×/i.test(b.textContent||'')); if(close) close.click(); }catch(e){}
    await sleep(250);
  }
  // 클릭으로 생긴 모달 등 행 밖의 새 요소는 제거(중복 파싱 방지)
  [...document.querySelectorAll('body *')].filter(e=>!initial.has(e) && !rows.some(r=>r.contains(e)) && e.parentElement && initial.has(e.parentElement)).forEach(e=>e.remove());
  resolve('rows='+rows.length+' expanded='+done);
})`

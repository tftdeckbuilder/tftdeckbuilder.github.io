package main

// MetaTFT 공식 comps API 직접 파싱 (2026-09-18)
// 사이트(www.metatft.com/comps)는 React 앱이라 HTML/DOM에는 덱 데이터가 없고, 아래 두 API가 데이터를 줍니다.
//   latest_cluster_info : 덱(클러스터) 목록 — units_string(유닛 ID들, 성급만큼 반복), name_string(덱 이름 코드)
//   comp_options        : 클러스터별·슬롯 수별 변형 목록 — count(표본 수), avg(평균 등수), num_unit_slots(레벨)
// 유닛 ID는 "DA_18_Varus", "DA_KogMaw18_AD", "DA_Nidalee18_AP", "DA_Sentinel18" 처럼 세트 18 접두사 DA_ 를 씁니다
// (TFT18_ 가 아님). 예전 텍스트 스캔 파서는 같은 유닛이 반복(성급)될 때 덱을 쪼개 버려 실패했습니다.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	mtClusterURL = "https://api-hc.metatft.com/tft-comps-api/latest_cluster_info"
	// 필터: 랭크 큐, 최신 패치(current), 최근 3일, 플래티넘 이상 (사용자 지정 09-18).
	// 참고: 사이트 /comps 기본값은 days=7 — 그 값으로 평균 등수·선택률·승률·순방 확률이 사이트 표시와 소수점까지 일치함을 실측.
	mtFilter      = "queue=1100&patch=current&days=3&rank=CHALLENGER,GRANDMASTER,MASTER,DIAMOND,EMERALD,PLATINUM&permit_filter_adjustment=true"
	mtFilterLabel = "최신 패치 · 최근 3일 · 플래티넘 이상"
)

func mtStatsURL(clusterID string) string {
	return "https://api-hc.metatft.com/tft-comps-api/comps_stats?" + mtFilter + "&cluster_id=" + clusterID
}

var mtTraitKo = map[string]string{
	"lunar": "달빛", "solar": "햇빛", "hunter": "사냥꾼", "sprykin": "날렵이", "vanguard": "선봉대", "blossom": "개화",
	"rapidfire": "속사포", "invoker": "기원자", "executioner": "처형자", "riftbeast": "협곡야수", "elderwood": "나무정령",
	"juggernaut": "전쟁기계", "fae": "요정", "primal": "원시", "adaptor": "적응가", "defender": "엄호대", "inferno": "지옥불",
	"blackthorn": "검은 가시", "spellweaver": "주문술사", "slayer": "약탈자", "coven": "악의 여단", "caustic": "부식",
	"bruiser": "싸움꾼", "summoner": "소환사", "rival": "경쟁자", "deadlybloom": "치명적인 꽃", "avatar": "화신",
	"monolith": "거석", "ancient": "고목", "attunement": "조율", "apexpredator": "최상위 포식자", "bountyhunter": "현상금 추적자",
}

var reMtIDStrip = regexp.MustCompile(`(?i)^(?:DA|TFT18)_(?:18_)?([A-Za-z]+?)(?:18)?(?:_(?:AP|AD|Base|Small|Blossom|Eldritch|Elderwood|Lunar|Coven|Fae|Primal|Inferno|Solar))?$`)

// "DA_KogMaw18_AD" → "kogmaw" (apiToKo / mtTraitKo 키)
func mtKey(id string) string {
	id = strings.TrimSpace(id)
	if m := reMtIDStrip.FindStringSubmatch(id); m != nil {
		return strings.ToLower(m[1])
	}
	s := strings.ToLower(id)
	s = strings.TrimPrefix(s, "da_")
	s = strings.TrimPrefix(s, "tft18_")
	s = strings.TrimPrefix(s, "18_")
	s = strings.ReplaceAll(s, "18", "")
	if i := strings.Index(s, "_"); i > 0 {
		s = s[:i]
	}
	return s
}

type mtCluster struct {
	Cluster     json.Number `json:"Cluster"`
	UnitsString string      `json:"units_string"`
	NameString  string      `json:"name_string"`
}

type mtClusterFile struct {
	ClusterInfo struct {
		ClusterDetails struct {
			Clusters []mtCluster `json:"clusters"`
		} `json:"cluster_details"`
	} `json:"cluster_info"`
}

// comp_details?comp=<cluster> — 사이트 /comps 한 줄이 쓰는 데이터
//
//	placements[0].avg   : 평균 등수 (필터 적용)
//	final_levels[]      : 마지막 레벨별 판수 → 가장 많은 레벨 = "빠른 8레벨/9레벨"
//	unit_stats[]        : 유닛별 채용률(pcnt)·성급별 비율 → 채용률 상위 N명 = 사이트가 보여주는 보드
type mtDetails struct {
	Results struct {
		Placements []struct {
			Count float64 `json:"count"`
			Avg   float64 `json:"avg"`
		} `json:"placements"`
		FinalLevels []struct {
			Level string  `json:"level"`
			Count float64 `json:"count"`
		} `json:"final_levels"`
		UnitStats []struct {
			Unit  string  `json:"unit"`
			Pcnt  float64 `json:"pcnt"`
			Count float64 `json:"count"`
			Tiers []struct {
				Tier int     `json:"tier"`
				Pcnt float64 `json:"pcnt"`
			} `json:"tiers"`
		} `json:"unit_stats"`
	} `json:"results"`
}

type mtStat struct {
	avg, pick, win, top4 float64 // comps_stats: 평균 등수, 선택률(한 판 8명 중 평균 몇 명), 1등 비율, 순방(4등 이내) 비율
	n                    float64
	hasStat              bool
	level                int                // comp_details: 판수가 가장 많은 마지막 레벨 (8/9)
	pcnt                 map[string]float64 // comp_details: 유닛 ID → 채용률
	three                map[string]bool    // comp_details: 3성 비율이 가장 높은 유닛
	hasDetail            bool
}

// comps_stats → 클러스터별 등수 분포. places[0..7]=1~8등 판수, places[8]=합. cluster "" 의 places[0]=전체 판수.
func mtParseStats(b []byte, into map[string]*mtStat) error {
	var f struct {
		Results []struct {
			Cluster string    `json:"cluster"`
			Places  []float64 `json:"places"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	var total float64
	for _, r := range f.Results {
		if r.Cluster == "" && len(r.Places) > 0 {
			total = r.Places[0]
		}
	}
	n := 0
	for _, r := range f.Results {
		if r.Cluster == "" || r.Cluster == "-1" || len(r.Places) < 9 || r.Places[8] <= 0 {
			continue
		}
		cnt := r.Places[8]
		var sum, top4 float64
		for i := 0; i < 8; i++ {
			sum += float64(i+1) * r.Places[i]
			if i < 4 {
				top4 += r.Places[i]
			}
		}
		st := into[r.Cluster]
		if st == nil {
			st = &mtStat{}
			into[r.Cluster] = st
		}
		st.avg, st.n, st.win, st.top4, st.hasStat = sum/cnt, cnt, r.Places[0]/cnt, top4/cnt, true
		if total > 0 {
			st.pick = cnt / total * 8
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("comps_stats 결과 없음")
	}
	return nil
}

func mtDetailsURL(cluster, clusterID string) string {
	return fmt.Sprintf("https://api-hc.metatft.com/tft-comps-api/comp_details?comp=%s&cluster_id=%s&%s", cluster, clusterID, mtFilter)
}

// comp_details → 레벨, 유닛별 채용률, 3성 여부
func mtParseDetails(b []byte, st *mtStat) error {
	var d struct {
		Results struct {
			FinalLevels []struct {
				Level string  `json:"level"`
				Count float64 `json:"count"`
			} `json:"final_levels"`
			UnitStats []struct {
				Unit  string  `json:"unit"`
				Pcnt  float64 `json:"pcnt"`
				Tiers []struct {
					Tier int     `json:"tier"`
					Pcnt float64 `json:"pcnt"`
				} `json:"tiers"`
			} `json:"unit_stats"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	r := d.Results
	if len(r.UnitStats) == 0 {
		return fmt.Errorf("unit_stats 없음")
	}
	bestN := -1.0
	for _, fl := range r.FinalLevels {
		var lv int
		fmt.Sscanf(fl.Level, "%d", &lv)
		if lv > 0 && fl.Count > bestN {
			bestN, st.level = fl.Count, lv
		}
	}
	if st.level > 9 {
		st.level = 9
	}
	st.pcnt = map[string]float64{}
	st.three = map[string]bool{}
	for _, u := range r.UnitStats {
		st.pcnt[mtKey(u.Unit)] = u.Pcnt
		bt, bp := 0, -1.0
		for _, t := range u.Tiers {
			if t.Pcnt > bp {
				bp, bt = t.Pcnt, t.Tier
			}
		}
		if bt >= 3 {
			st.three[mtKey(u.Unit)] = true
		}
	}
	st.hasDetail = true
	return nil
}

// latest_cluster_info(+comp_options 통계) → 덱 목록
func mtParseClusters(b []byte, stats map[string]*mtStat) ([]labsComp, error) {
	var f mtClusterFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("cluster JSON 해석 실패: %v", err)
	}
	cl := f.ClusterInfo.ClusterDetails.Clusters
	if len(cl) == 0 {
		return nil, fmt.Errorf("clusters 비어 있음")
	}
	type row struct {
		comp  labsComp
		avg   float64
		idx   int
		hasAv bool
	}
	var rows []row
	for i, c := range cl {
		cid := c.Cluster.String()
		st, ok := stats[cid]
		// 보드 = 사이트가 보여주는 유닛: 클러스터 유닛 목록(units_string) 중 채용률이 낮은 유닛을 빼고 레벨 수만큼
		// (실측: 달빛 아펠리오스 units_string 9명 중 알룬 채용률 14% → 사이트는 8명 표시)
		var order, three []string
		seen := map[string]bool{}
		for _, id := range strings.Split(c.UnitsString, ",") {
			k := mtKey(id)
			ko, known := apiToKo[k]
			if !known || seen[ko] {
				continue
			}
			if ok && st.hasDetail {
				if p, has := st.pcnt[k]; has && p < 0.3 {
					continue
				}
				if st.three[k] {
					three = append(three, ko)
				}
			}
			seen[ko] = true
			order = append(order, ko)
		}
		if ok && st.hasDetail && st.level >= 6 && len(order) > st.level {
			// 채용률 낮은 순으로 잘라 레벨 수에 맞춤
			type uc struct {
				ko string
				p  float64
			}
			var ucs []uc
			for _, ko := range order {
				p := 1.0
				for _, id := range strings.Split(c.UnitsString, ",") {
					if k := mtKey(id); apiToKo[k] == ko {
						if v, has := st.pcnt[k]; has {
							p = v
						}
						break
					}
				}
				ucs = append(ucs, uc{ko, p})
			}
			sort.SliceStable(ucs, func(i, j int) bool { return ucs[i].p > ucs[j].p })
			keep := map[string]bool{}
			for i := 0; i < st.level && i < len(ucs); i++ {
				keep[ucs[i].ko] = true
			}
			var trimmed []string
			for _, ko := range order {
				if keep[ko] {
					trimmed = append(trimmed, ko)
				}
			}
			order = trimmed
		}
		if len(order) < 6 {
			continue
		}
		// 이름: 특성 코드 + 유닛 코드
		var parts []string
		for _, id := range strings.Split(c.NameString, ",") {
			k := mtKey(id)
			if t, ok := mtTraitKo[k]; ok {
				parts = append(parts, t)
			} else if u, ok := apiToKo[k]; ok {
				parts = append(parts, u)
			} else if k != "" {
				parts = append(parts, strings.Title(k))
			}
			if len(parts) >= 2 {
				break
			}
		}
		name := strings.Join(parts, " ")
		if name == "" {
			name = order[0] + " 덱"
		}
		r := row{comp: labsComp{Name: name, Units: order, Tier: "?"}, idx: i}
		var parts2 []string
		level := 0
		if ok && st.hasDetail {
			level = st.level
		}
		if level >= 8 {
			parts2 = append(parts2, fmt.Sprintf("빠른 %d레벨", level))
		} else if level > 0 {
			if len(three) > 0 {
				parts2 = append(parts2, fmt.Sprintf("%d레벨 리롤", level))
			} else {
				parts2 = append(parts2, fmt.Sprintf("%d레벨", level))
			}
		}
		if len(three) > 0 {
			parts2 = append(parts2, "3성 "+strings.Join(three, "·"))
		}
		if ok && st.hasStat {
			r.avg, r.hasAv = st.avg, true
			parts2 = append(parts2, fmt.Sprintf("평균 %.2f", st.avg), fmt.Sprintf("선택률 %.2f", st.pick), fmt.Sprintf("승률 %.1f%%", st.win*100), fmt.Sprintf("순방 %.1f%%", st.top4*100))
		}
		r.comp.Code = strings.Join(parts2, " · ")
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("인식된 덱 없음 (유닛 ID 형식 변경?)")
	}
	// 티어: 평균 등수 순위(있으면) 또는 목록 순서
	withAvg := 0
	for _, r := range rows {
		if r.hasAv {
			withAvg++
		}
	}
	if withAvg*2 >= len(rows) {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].hasAv != rows[j].hasAv {
				return rows[i].hasAv
			}
			if rows[i].hasAv {
				return rows[i].avg < rows[j].avg
			}
			return rows[i].idx < rows[j].idx
		})
	}
	n := len(rows)
	for i := range rows {
		if rows[i].hasAv {
			a := float64(int(rows[i].avg*100+0.5)) / 100 // 사이트 표시(소수 2자리) 기준
			switch {
			case a <= 4.25:
				rows[i].comp.Tier = "S"
			case a <= 4.50:
				rows[i].comp.Tier = "A"
			case a <= 4.75:
				rows[i].comp.Tier = "B"
			default:
				rows[i].comp.Tier = "C"
			}
			continue
		}
		p := float64(i) / float64(n)
		switch {
		case i < 4 || p < 0.10:
			rows[i].comp.Tier = "S"
		case p < 0.40:
			rows[i].comp.Tier = "A"
		case p < 0.75:
			rows[i].comp.Tier = "B"
		default:
			rows[i].comp.Tier = "C"
		}
	}
	out := make([]labsComp, 0, n)
	for _, r := range rows {
		out = append(out, r.comp)
	}
	return out, nil
}

// 브라우저(headless Edge/Chrome) 안에서 fetch — Go 클라이언트가 봇 차단/TLS 지문으로 거절될 때의 우회.
// metatft.com 페이지 컨텍스트에서 실행하므로 CORS·쿠키가 사이트와 동일하게 적용된다.
var mtPageURL = "https://www.metatft.com/comps"

func mtFetchViaBrowser(apiURL string) ([]byte, error) {
	sess, err := openCDP(mtPageURL)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	js := fmt.Sprintf(`fetch(%q,{credentials:'include'}).then(async r=>'HTTP '+r.status+'\n'+await r.text()).catch(e=>'ERR '+e)`, apiURL)
	out, err := sess.eval(js, 120*time.Second)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(out, "ERR ") {
		return nil, fmt.Errorf("%s", firstN(out, 200))
	}
	if !strings.HasPrefix(out, "HTTP 200\n") {
		return nil, fmt.Errorf("%s", firstN(out, 120))
	}
	return []byte(out[len("HTTP 200\n"):]), nil
}

// 직접 GET → 실패하면 브라우저 fetch. 어떤 단계가 왜 실패했는지 로그로 남긴다.
func mtGet(client *http.Client, apiURL string, minLen int) ([]byte, string, error) {
	b, code, err := httpGet(client, apiURL)
	if err == nil && code == 200 && len(b) >= minLen && json.Valid(b) {
		return b, "direct", nil
	}
	why := ""
	if err != nil {
		why = err.Error()
	} else {
		why = fmt.Sprintf("HTTP %d, %d바이트, %s", code, len(b), firstN(strings.TrimSpace(string(b)), 100))
	}
	logf("MetaTFT: 직접 요청 실패 %s → %s / 브라우저로 재시도", apiURL, why)
	bb, berr := mtFetchViaBrowser(apiURL)
	if berr != nil {
		return nil, "", fmt.Errorf("직접 요청: %s; 브라우저 요청: %v", why, berr)
	}
	if len(bb) < minLen || !json.Valid(bb) {
		return nil, "", fmt.Errorf("직접 요청: %s; 브라우저 응답이 JSON이 아님(%d바이트): %s", why, len(bb), firstN(string(bb), 100))
	}
	return bb, "browser", nil
}

// 클러스터 목록 → comps_stats(사이트 필터, 1회) → 클러스터마다 comp_details(레벨·채용률·3성)
func updateMetatftAPI(client *http.Client) ([]labsComp, error) {
	b, how, err := mtGet(client, mtClusterURL, 500)
	if err != nil {
		return nil, fmt.Errorf("latest_cluster_info: %v", err)
	}
	logf("MetaTFT: latest_cluster_info %d바이트 (%s)", len(b), how)
	writeDebug("metatft_cluster.json", b)
	var f struct {
		ClusterInfo struct {
			ClusterID      json.Number `json:"cluster_id"`
			ClusterDetails struct {
				Clusters []mtCluster `json:"clusters"`
			} `json:"cluster_details"`
		} `json:"cluster_info"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("cluster JSON 해석 실패: %v", err)
	}
	clusterID := f.ClusterInfo.ClusterID.String()
	stats := map[string]*mtStat{}
	if sb, show, serr := mtGet(client, mtStatsURL(clusterID), 200); serr == nil {
		writeDebug("metatft_stats.json", sb)
		if perr := mtParseStats(sb, stats); perr != nil {
			logf("MetaTFT: comps_stats 해석 실패: %v", perr)
		} else {
			logf("MetaTFT: comps_stats %d바이트 (%s), %d클러스터", len(sb), show, len(stats))
		}
	} else {
		logf("MetaTFT: comps_stats 실패: %v — 티어는 목록 순서로", serr)
	}
	fails := 0
	okN := 0
	for i, c := range f.ClusterInfo.ClusterDetails.Clusters {
		cid := c.Cluster.String()
		db, _, derr := mtGet(client, mtDetailsURL(cid, clusterID), 200)
		if derr != nil {
			fails++
			logf("MetaTFT: comp_details %s 실패: %v", cid, derr)
			if fails >= 3 && okN == 0 {
				break
			}
			continue
		}
		if i == 0 {
			writeDebug("metatft_details_0.json", db)
		}
		st := stats[cid]
		if st == nil {
			st = &mtStat{}
			stats[cid] = st
		}
		if perr := mtParseDetails(db, st); perr != nil {
			logf("MetaTFT: comp_details %s 해석 실패: %v", cid, perr)
			continue
		}
		okN++
	}
	logf("MetaTFT: comp_details %d/%d개 (실패 %d)", okN, len(f.ClusterInfo.ClusterDetails.Clusters), fails)
	cs, err := mtParseClusters(b, stats)
	if err != nil {
		return nil, err
	}
	logf("MetaTFT API: 덱 %d개", len(cs))
	return cs, nil
}

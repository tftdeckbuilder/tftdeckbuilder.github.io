# 세트 18 덱 빌더 — 웹사이트

`index.html` 한 파일이 사이트 전체입니다. `data/` 폴더의 메타 덱 목록은 GitHub Actions가 매일 새벽 5시(한국 시간)에 자동으로 갱신합니다.

## 처음 올리기 (5분)

1. GitHub에 로그인 → 오른쪽 위 **+** → **New repository** → 이름 `tft` (아무 이름이나 됨), **Public**, Create.
2. 만들어진 저장소 페이지에서 **uploading an existing file** 클릭 → 이 zip을 푼 폴더 안의 내용물(`index.html`, `data`, `updater`, `.github`, `README.md`, `.gitignore`)을 전부 끌어다 놓고 **Commit changes**.
   - `.github` 폴더는 숨김 폴더라 안 보이면 탐색기에서 "숨긴 항목 표시"를 켜세요. 드래그가 안 되면 GitHub Desktop 이나 `git` 으로 올려도 됩니다.
3. 저장소 **Settings → Pages** → Source: **Deploy from a branch**, Branch: **main** / **(root)** → Save.
4. 1~2분 뒤 `https://<아이디>.github.io/tft/` 로 접속.

## 자동 업데이트 켜기

- 저장소 **Settings → Actions → General → Workflow permissions** 에서 **Read and write permissions** 선택 → Save. (이걸 안 하면 갱신한 파일을 저장소에 쓰지 못합니다.)
- **Actions** 탭 → "메타 덱 자동 업데이트" → **Run workflow** 로 한 번 직접 돌려서 초록색 체크가 뜨는지 확인. 이후로는 매일 자동.
- 페이지의 메타 덱 패널 오른쪽에 `MetaTFT · 2026-09-12` 처럼 갱신 날짜가 표시됩니다.

## 파일 설명

| 경로 | 내용 |
|---|---|
| `index.html` | 덱 빌더 페이지 전체 (유닛·삼신기·상징·팀 코드 포함) |
| `data/meta_metatft.txt` | MetaTFT 메타 덱. 형식 `티어 \| 이름 \| 유닛, … \| 메모`, 첫 줄 `# source: 출처 · 날짜` |
| `data/meta_tftlabs.txt` | tftlabs 메타 덱 |
| `data/meta.txt` | tftactics 메타 덱 |
| `updater/` | 업데이트 프로그램(Go). exe 버전과 같은 소스. `-update all -data data` 로 실행하면 서버 없이 갱신만 하고 종료 |
| `.github/workflows/update-meta.yml` | 매일 자동 실행 설정 (시간을 바꾸려면 `cron` 줄 수정, UTC 기준) |

## 손으로 고치기

`data/*.txt` 를 GitHub 웹에서 직접 편집해도 됩니다(연필 아이콘). 한 줄에 한 덱, 유닛은 한글·영문 모두 인식합니다.

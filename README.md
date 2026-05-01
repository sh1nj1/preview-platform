# Preview Platform

Traefik v3 기반 self-hosted preview environment 템플릿. 와일드카드 도메인 한 번 설정으로 **git worktree 병렬 작업**과 **PR 자동 프리뷰**를 모두 처리합니다.

핵심: 컨트롤 플레인 코드를 직접 짜지 않습니다. Traefik의 두 provider(Docker 라벨, File watch)가 라우팅을 담당하고, 작은 Go API 하나(`preview-api`)가 어디서든 호출 가능한 등록 인터페이스를 제공합니다.

## 무엇을 해주나

- `*.preview.example.com` 와일드카드 HTTPS (Let's Encrypt + Route 53 DNS-01)
- worktree 로컬 dev 서버: `preview link` 한 줄로 등록 — **로컬 / LAN PC / Tailscale 노트북 어디서든 동일하게**
- PR 자동 프리뷰: GitHub Actions에서 라벨 단 컨테이너 띄우면 끝
- Traefik 대시보드 (BasicAuth)
- Claude Code 스킬 포함 (`skills/preview/SKILL.md`) — AI에게 "preview link 생성해줘"라고 시키면 자동 실행

## 구조

```
preview-platform/
├── docker-compose.yml         # Traefik v3 + preview-api
├── Dockerfile.api             # preview-api 멀티아치 빌드 (CLI 바이너리 임베딩)
├── Makefile                   # 로컬 빌드 (make build)
├── go.mod
├── cmd/
│   ├── api/                   # Go: 등록 API 서버
│   └── preview/               # Go: preview CLI
├── skills/
│   └── preview/SKILL.md       # Claude Code 스킬 (캐노니컬)
├── .env.example               # 도메인, AWS 키, 대시보드 비밀번호, API 토큰
├── install.sh                 # 호스트 부트스트랩
├── dynamic/                   # File provider watch 디렉토리
├── letsencrypt/               # ACME 저장소 (acme.json)
└── examples/                  # PR 자동 프리뷰 예제
```

## 사전 준비

- 서버 한 대 (Ubuntu 22.04+, RAM 2GB+, 80/443 포트 노출)
- Route 53에서 관리되는 도메인
- AWS IAM 키 (Route 53 권한: `ChangeResourceRecordSets`, `ListHostedZonesByName`, `GetChange`)
- Docker (`install.sh`가 없으면 설치)

## 빠른 시작 — 서버

### 1. DNS

Route 53에 와일드카드 A 레코드 한 줄:

```
*.preview.example.com   A   <서버 공인 IP>   TTL 300
```

`traefik.*`, `api.*`, 그리고 모든 worktree 서브도메인이 이 한 줄로 커버됩니다.

### 2. 부트스트랩

```bash
git clone <this-repo> preview-platform
cd preview-platform
sudo ./install.sh
```

### 3. 환경 설정

```bash
cd /srv/preview-platform
nano .env
```

채워야 할 항목:
- `PREVIEW_DOMAIN` — 예: `preview.회사.com`
- `ACME_EMAIL` — Let's Encrypt 알림용
- `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_HOSTED_ZONE_ID`
- `DASHBOARD_AUTH` — `htpasswd -nb admin yourpw` 결과 (`$`를 `$$`로 escape)
- **`PREVIEW_API_TOKEN`** — `openssl rand -hex 32`로 생성. 이 토큰을 가진 사람만 preview link를 만들 수 있음

### 4. 기동

```bash
docker compose up -d --build
docker compose logs -f traefik | grep -i acme
```

첫 와일드카드 인증서 발급에 30~60초. 완료 후:
- `https://traefik.<도메인>` — 대시보드 (BasicAuth)
- `https://api.<도메인>/healthz` — preview-api 헬스체크 (인증 불필요)

## 빠른 시작 — 개발자 머신

서버 부트스트랩 후, 각 개발자는 자기 머신에서 한 줄로 CLI 설치:

```bash
TOKEN="<PREVIEW_API_TOKEN 값>"
curl -fsSL -H "Authorization: Bearer $TOKEN" \
  https://api.<도메인>/install.sh | bash
# Claude Code 스킬도 같이 설치하려면:
curl -fsSL -H "Authorization: Bearer $TOKEN" \
  https://api.<도메인>/install.sh | bash -s -- --with-skill
```

설치 결과:
- `~/.local/bin/preview` — CLI 바이너리 (자동으로 OS/arch 감지)
- `~/.config/preview/config` — endpoint + token 저장
- (옵션) `~/.claude/skills/preview/SKILL.md` — Claude Code 스킬

`~/.local/bin`이 PATH에 있는지 확인:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

## 사용법

### Worktree 워크플로

`preview link`는 **어디서 실행하느냐에 따라 upstream을 자동 감지**합니다:

- **서버 본체**: 서버의 default route IP
- **사내망 PC**: 사내망 IP (서버에서 도달 가능해야 함)
- **Tailscale 노트북**: `tailscale ip -4` 결과 자동 사용

```bash
# 1) worktree 생성
git worktree add ../wt-feature-auth feature/auth
cd ../wt-feature-auth

# 2) 등록 — branch → slug, 빈 포트 자동 할당
preview link
# https://feature-auth.myapp.preview.example.com   ← stdout (URL만)
# ✓ linked                                          ← stderr
#   upstream: http://100.64.0.5:3014
#   env:      .preview.env (PORT=3014)

# 3) 그 포트로 dev 서버 (반드시 0.0.0.0 바인딩)
source .preview.env
npm run dev -- --host 0.0.0.0

# 4) 끝나면 정리
preview unlink
git worktree remove ../wt-feature-auth
```

명령:

```
preview link    [--port N] [--upstream URL] [--slug NAME]
preview unlink  [--slug NAME]
preview list    [--all]               # 기본: 현재 프로젝트만
preview url     [--slug NAME]
```

환경변수 / 설정:

```
~/.config/preview/config       endpoint=, token= (install.sh가 생성)
PREVIEW_API                    엔드포인트 override
PREVIEW_API_TOKEN              토큰 override
PREVIEW_HOST_IP                upstream IP 자동 감지 끄기
PREVIEW_PORT_START / _END      포트 풀 (기본 3001~3099)
```

### PR 자동 프리뷰

`examples/github-actions-pr.yml`을 앱 레포의 `.github/workflows/preview.yml`로 복사. 이 흐름은 **API를 거치지 않고** Traefik의 Docker provider가 라벨로 직접 라우팅합니다.

레포에 추가할 GitHub secrets:
- `PREVIEW_HOST`, `PREVIEW_USER`, `PREVIEW_SSH_KEY` — 프리뷰 서버 SSH
- `PREVIEW_DOMAIN`

PR 열면 컨테이너 빌드 후 PR 코멘트로 URL 자동 게시. PR 닫으면 정리.

도메인 패턴: `pr-<번호>.<레포명>.preview.example.com`

### Claude Code 스킬

`--with-skill`로 설치했다면, Claude Code 세션에서 "이 브랜치 preview 만들어줘" 같은 자연어 요청에 자동으로 `preview link`를 호출합니다. 스킬 정의는 `skills/preview/SKILL.md` 참고.

## 동작 원리

```
            Browser                      개발자 머신
               │ HTTPS                       │
               ▼                             │ HTTPS POST /v1/previews
          Traefik :443                       ▼
          ├── Docker provider  ──── PR 컨테이너 (라벨)
          └── File provider    ◀── /dynamic/*.yml ◀── preview-api
                                                          (api.<도메인>)
```

- **와일드카드 인증서** 한 장이 모든 서브도메인 커버 (api.* 포함)
- **File provider**가 `dynamic/`을 watch → preview-api가 YAML을 쓰면 즉시 라우팅 반영
- **Docker provider**가 컨테이너 라벨 → PR 컨테이너 라이프사이클 = 라우트 라이프사이클
- **preview-api**는 Bearer 토큰만 검증, dynamic/ 디렉토리에 파일을 쓰는 것 외엔 상태 없음

## HTTPS · Let's Encrypt · Route 53 — 왜 AWS access key가 필요한가

짧게: **`*.preview.example.com` 와일드카드 인증서를 자동으로 발급받기 위해서**입니다. AWS 키는 EC2/S3 같은 호스팅에 쓰는 게 아니라, Traefik이 Let's Encrypt에게 도메인 소유권을 증명할 때 **Route 53에 TXT 레코드를 임시로 추가**할 권한이 필요해서 씁니다.

### 왜 와일드카드 인증서인가

`feature-auth.myapp.preview.example.com`, `pr-42.myrepo.preview.example.com` 처럼 서브도메인이 동적으로 생성됩니다. 매번 발급 받으면:

- Let's Encrypt rate limit (도메인당 주 50장)에 금방 걸림
- 첫 요청 시 발급 지연(수 초~십수 초)
- 모든 서브도메인이 CT 로그에 공개됨 (preview URL 노출)

`*.preview.example.com` 한 장으로 미리 발급하면 위 셋 다 해결됩니다.

### 왜 DNS-01 챌린지인가

Let's Encrypt가 도메인 소유권을 검증하는 방법은 셋:

| 챌린지 | 어떻게 검증 | 와일드카드 가능? | 추가 자격증명 |
|---|---|---|---|
| HTTP-01 | LE가 80포트로 접속해 검증 파일 가져감 | ❌ | 없음 |
| TLS-ALPN-01 | LE가 443포트로 검증 | ❌ | 없음 |
| **DNS-01** | `_acme-challenge.<도메인>`에 TXT 레코드 추가 | ✅ | **DNS API 키** |

와일드카드는 DNS-01만 지원합니다. 그래서 Traefik이 자동으로 TXT 레코드를 추가/삭제할 수 있게 DNS 프로바이더의 API 자격증명이 필요합니다 — Route 53을 쓰니까 AWS access key/secret.

### 필요한 IAM 권한 (최소)

```
route53:ChangeResourceRecordSets
route53:ListHostedZonesByName
route53:GetChange
```

이게 다입니다. EC2, S3 등 다른 권한은 일절 불필요. `AWS_HOSTED_ZONE_ID`로 zone을 명시하면 `route53:ListHostedZonesByName`도 빼도 됩니다.

### 동작 흐름

```
[발급/갱신 시점만]                  [평소]
Traefik ─ ACME ─▶ Let's Encrypt    Browser ─▶ Traefik ─▶ upstream
   │                  │
   │  "이 도메인       │  "_acme-challenge.preview.example.com
   │   소유 증명해"    │   에 ABC123 TXT 추가하세요"
   ▼                  ▼
Route 53 API ◀── TXT 레코드 추가 ── Traefik
   │
   ▼  (검증 성공)
Let's Encrypt ─▶ 와일드카드 cert 발급 ─▶ acme.json 저장
```

- 평상시 트래픽은 AWS와 무관 — Traefik과 upstream만 거침
- AWS API는 **인증서 발급(60~90일에 한 번) + 갱신** 시에만 호출, 비용/레이턴시 모두 무시 수준
- 서버 호스팅은 어디든 OK (Hetzner, 사내 VM 등) — DNS만 Route 53이면 됨

### Route 53이 아닌 DNS를 쓰고 싶다면

Cloudflare, Google Cloud DNS, RFC 2136(BIND) 등 [Traefik이 지원하는 DNS provider](https://doc.traefik.io/traefik/https/acme/#providers)는 모두 같은 패턴입니다 — 그쪽 API 토큰을 환경변수로 주고 `dnschallenge.provider`를 바꾸면 됩니다. AWS access key는 Route 53 한정.

### AWS access key 없이 가는 방법 (트레이드오프)

- **HTTP-01로 전환** + 와일드카드 포기: 서브도메인마다 인증서 따로 발급. rate limit · CT 로그 노출 감수.
- **EC2 위에 호스팅 + IAM Role**: access key 대신 인스턴스 프로파일. AWS 외부에선 불가.
- **DNS A 레코드 와일드카드만 쓰고 인증서는 self-signed**: 사내 전용일 때.

기본 권장은 지금 구성(Route 53 + DNS-01 + access key)입니다. 키 권한이 매우 좁고, 다른 모든 패턴보다 운영 부담이 가장 적습니다.

## 로컬 개발 (preview-api 자체)

```bash
make build              # bin/preview, bin/preview-api 생성
make api                # API만
make cli                # CLI만
make docker             # Docker 이미지 빌드
```

API 로컬 실행:

```bash
PREVIEW_API_TOKEN=devtoken \
PREVIEW_DOMAIN=preview.example.com \
DYNAMIC_DIR=./dynamic \
LISTEN_ADDR=:8080 \
PREVIEW_PUBLIC_API_URL=http://localhost:8080 \
  ./bin/preview-api
```

## 보안 체크리스트

- [ ] 대시보드(`traefik.<domain>`) BasicAuth 또는 IP allowlist 적용 — 기본 BasicAuth 포함
- [ ] **`PREVIEW_API_TOKEN`은 무작위 32바이트 이상**. 유출 시 누구나 임의 라우트 등록 가능
- [ ] preview 도메인을 사내망/VPN 뒤에 두거나, oauth2-proxy + `forwardAuth` 미들웨어로 SSO 적용
- [ ] `.env`와 `letsencrypt/acme.json` 권한 600
- [ ] PR 컨테이너 시크릿은 GitHub Actions secrets로만 주입
- [ ] 사내 도메인이면 Route 53 Private Hosted Zone 고려 (외부 노출 차단)

## 트러블슈팅

**인증서 발급 실패** — `docker compose logs traefik | grep -i acme`. AWS 키가 Route 53에 도달하는지, hosted zone ID가 정확한지(여러 개면 ID 명시 필요), 도메인이 실제로 그 zone 안에 있는지 확인.

**`preview link`가 401** — 토큰이 틀렸거나, `~/.config/preview/config`가 안 읽힘. `PREVIEW_API_TOKEN=... preview link`로 임시 검증.

**라우트가 안 잡힘** — `docker compose logs preview-api`로 등록은 됐는지, `ls /srv/preview-platform/dynamic/`로 YAML이 실제로 생겼는지, traefik이 그 파일을 읽을 수 있는지(`docker compose logs traefik | grep file`).

**대시보드가 비밀번호를 거부** — `.env` 안에서 `$`가 모두 `$$`로 escape되어 있는지. `htpasswd -nb`가 만든 그대로 넣으면 docker compose가 변수 치환을 시도해 깨집니다.

**`preview link` 후 URL이 안 열림** — dev 서버가 정확히 `.preview.env`의 PORT에 바인딩됐는지(`ss -tln | grep <PORT>`). 그리고 dev 서버가 `0.0.0.0`이 아니라 `localhost`에만 바인딩되면 Traefik에서 도달 못 합니다 — 보통 `--host 0.0.0.0` 같은 플래그 필요.

**Tailscale 노트북에서 upstream이 LAN IP로 잡힘** — `tailscale ip -4`가 동작하는지 확인. 안 되면 `preview link --upstream http://100.x.x.x:PORT`로 명시.

**Vite/Next dev 서버에서 HMR 안 됨** — 거의 항상 `host` 헤더 검사 때문. Vite는 `server.allowedHosts`에 와일드카드 추가, Next는 보통 그대로 동작.

## 비용

- 와일드카드 인증서: 무료 (Let's Encrypt)
- Route 53: hosted zone $0.50/월 + 쿼리당 미세
- 서버: 어디든 (AWS Lightsail, Hetzner, 사내 VM)
- AWS API 호출: DNS-01 챌린지는 발급/갱신 시(60~90일에 한 번)에만 발생, 무시할 수준

## 라이선스

이 템플릿은 자유롭게 사용/수정. Traefik 자체는 MIT.

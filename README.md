# Preview Platform

Traefik v3 기반 self-hosted preview environment 템플릿. 와일드카드 도메인 한 번 설정으로 **git worktree 병렬 작업**과 **PR 자동 프리뷰**를 모두 처리합니다.

핵심: 컨트롤 플레인 코드를 직접 짜지 않습니다. Traefik의 두 provider(Docker 라벨, File watch)가 그 역할을 모두 대신합니다.

## 무엇을 해주나

- `*.preview.example.com` 와일드카드 HTTPS (Let's Encrypt + Route 53 DNS-01)
- worktree 로컬 dev 서버: `preview link` 한 줄로 등록
- PR 자동 프리뷰: GitHub Actions에서 라벨 단 컨테이너 띄우면 끝
- Traefik 대시보드 (BasicAuth)
- 직접 만드는 코드: 단일 셸 스크립트(`bin/preview`) ~150줄

## 구조

```
preview-platform/
├── docker-compose.yml         # Traefik v3 (와일드카드 인증서 포함)
├── .env.example               # 도메인, AWS 키, 대시보드 비밀번호
├── install.sh                 # 호스트 부트스트랩
├── bin/
│   └── preview                # worktree 등록/해제 CLI
├── dynamic/                   # File provider watch 디렉토리 (worktree용)
├── letsencrypt/               # ACME 저장소 (acme.json)
└── examples/
    ├── pr-app-compose.yml     # PR 컨테이너 docker-compose
    └── github-actions-pr.yml  # PR 자동 프리뷰 워크플로
```

## 사전 준비

- 서버 한 대 (Ubuntu 22.04+, RAM 2GB+, 80/443 포트 노출)
- Route 53에서 관리되는 도메인
- AWS IAM 키 (Route 53 권한: `ChangeResourceRecordSets`, `ListHostedZonesByName`, `GetChange`)
- Docker (`install.sh`가 없으면 설치)

## 빠른 시작

### 1. DNS

Route 53에 와일드카드 A 레코드 추가:

```
*.preview.example.com   A   <서버 공인 IP>   TTL 300
```

### 2. 서버 부트스트랩

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
- `DASHBOARD_AUTH` — `htpasswd -nb admin yourpw` 결과를 넣되 **`$`를 모두 `$$`로 escape**

### 4. 기동

```bash
cd /srv/preview-platform
docker compose up -d
docker compose logs -f traefik | grep -i acme   # 인증서 발급 로그
```

첫 와일드카드 인증서 발급에 30~60초. 완료되면 `https://traefik.<your-domain>`에 BasicAuth로 대시보드 접속.

## 사용법

### Worktree 워크플로

개발 머신에서 `preview` CLI 사용. 단, **`/srv/preview-platform/dynamic` 에 쓸 수 있어야** 합니다 — 같은 머신이면 그룹 권한, 다른 머신이면 sshfs 마운트.

```bash
# 1) worktree 생성
git worktree add ../wt-feature-auth feature/auth
cd ../wt-feature-auth

# 2) 등록 — branch 이름 → slug, 빈 포트 자동 할당
preview link
# ✓ linked
#   URL:      https://feature-auth.myapp.preview.example.com
#   upstream: http://192.168.1.50:3014
#   env:      .preview.env (PORT=3014)

# 3) 그 포트로 dev 서버 띄우기
source .preview.env
npm run dev          # 또는 rails s, dotnet run, ...
# direnv 사용자: .envrc에 'dotenv .preview.env' 한 줄

# 4) 끝나면 정리
preview unlink
git worktree remove ../wt-feature-auth
```

기타 명령:

```
preview link [slug] [port]   # slug/포트 명시 가능
preview unlink [slug]
preview list                 # 이 프로젝트의 활성 프리뷰
preview url [slug]           # URL만 출력
```

환경변수로 동작 변경:

```
PREVIEW_DOMAIN          base 도메인
PREVIEW_DYNAMIC_DIR     Traefik dynamic 디렉토리
PREVIEW_PORT_START      포트 풀 시작 (기본 3001)
PREVIEW_PORT_END        포트 풀 끝   (기본 3099)
PREVIEW_HOST_IP         자동 감지 IP 덮어쓰기
```

### PR 자동 프리뷰

`examples/github-actions-pr.yml`을 앱 레포의 `.github/workflows/preview.yml`로 복사.

레포에 추가할 GitHub secrets:
- `PREVIEW_HOST`, `PREVIEW_USER`, `PREVIEW_SSH_KEY` — 프리뷰 서버 SSH
- `PREVIEW_DOMAIN`

레포에 추가할 GitHub variables (선택):
- `APP_PORT` — 컨테이너가 듣는 포트 (기본 3000)

PR 열면 5~10분 후(빌드 시간) PR 코멘트로 URL이 자동으로 달립니다. PR 닫으면 컨테이너 + 라우트 자동 정리.

생성되는 도메인 패턴: `pr-<번호>.<레포명>.preview.example.com`

### 같은 도메인 안에서 worktree와 PR 공존

- worktree → `<slug>.<프로젝트>.preview.example.com`
- PR       → `pr-<번호>.<레포명>.preview.example.com`

같은 Traefik이 두 종류를 모두 라우팅합니다(File provider + Docker provider 동시).

## 동작 원리

```
                Browser
                   │ HTTPS
                   ▼
              Traefik :443
              ├── Docker provider  ── PR 컨테이너 (라벨)
              └── File provider    ── worktree dev 서버 (dynamic/*.yml)
```

- **와일드카드 인증서** 한 장이 모든 서브도메인을 커버 → 새 라우트마다 인증서 발급 안 함
- **File provider**가 `dynamic/`을 watch → 파일 추가/삭제 시 즉시 반영, reload 불필요
- **Docker provider**가 컨테이너 라벨 → 컨테이너 라이프사이클 = 라우트 라이프사이클

## 보안 체크리스트

- [ ] 대시보드(`traefik.<domain>`) BasicAuth 또는 IP allowlist 적용 — 기본 BasicAuth 포함
- [ ] preview 도메인을 사내망/VPN 뒤에 두거나, oauth2-proxy + `forwardAuth` 미들웨어로 SSO 적용
- [ ] `.env`와 `letsencrypt/acme.json` 권한 600
- [ ] PR 컨테이너 시크릿은 GitHub Actions secrets로만 주입
- [ ] 사내 도메인이면 Route 53 Private Hosted Zone 고려 (외부 노출 차단)

## 트러블슈팅

**인증서 발급 실패** — `docker compose logs traefik | grep -i acme`. AWS 키가 Route 53에 도달하는지, hosted zone ID가 정확한지(여러 개면 ID 명시 필요), 도메인이 실제로 그 zone 안에 있는지 확인.

**라우트가 안 잡힘** — Docker 컨테이너의 경우 `traefik.docker.network=preview` 라벨이 있는지, 컨테이너가 `preview` 네트워크에 붙었는지. File provider면 `dynamic/*.yml` 권한이 traefik 컨테이너에서 읽힐 수 있는지.

**대시보드가 비밀번호를 거부** — `.env` 안에서 `$`가 모두 `$$`로 escape되어 있는지. `htpasswd -nb`가 만든 그대로 넣으면 docker compose가 변수 치환을 시도해 깨집니다.

**`preview link` 후 URL이 안 열림** — `.preview.env` 로드 후 dev 서버가 정확히 그 PORT에 바인딩됐는지(`ss -tln | grep <PORT>`). 그리고 dev 서버가 `0.0.0.0`이 아니라 `localhost`에만 바인딩되면 Traefik에서 도달 못 합니다 — 보통 `--host 0.0.0.0` 같은 플래그 필요.

**Vite/Next dev 서버에서 HMR 안 됨** — 거의 항상 `host` 헤더 검사 때문. Vite는 `server.allowedHosts`에 와일드카드 추가, Next는 보통 그대로 동작.

## 비용

- 와일드카드 인증서: 무료 (Let's Encrypt)
- Route 53: hosted zone $0.50/월 + 쿼리당 미세
- 서버: 어디든 (AWS Lightsail, Hetzner, 사내 VM)
- AWS API 호출: DNS-01 챌린지는 발급/갱신 시(60~90일에 한 번)에만 발생, 무시할 수준

## 라이선스

이 템플릿은 자유롭게 사용/수정. Traefik 자체는 MIT.

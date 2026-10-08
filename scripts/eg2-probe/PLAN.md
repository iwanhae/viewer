# EG2 Overnight Study — GB10 (DGX Spark) 실험 계획서

> 이 문서는 오케스트레이터(메인 에이전트)가 compaction 후에도 그대로 실행을 이어갈 수
> 있도록 모든 컨텍스트를 담는 단일 진실 공급원이다. Phase 실행은 서브에이전트가 대리
> 수행하고, 오케스트레이터는 결과 취합과 리포트 작성만 한다.

## 1. 배경과 목표

- 토이 프로젝트 `viewer`(개인 사진 서버, Go)는 현재 SigLIP2
  (`google/siglip2-base-patch16-224`)로 이미지 임베딩 → 이미지-이미지 유사 추천.
- `google/embeddinggemma-2`(EG2, 740M, 멀티모달, MRL 768/512/256/128d)로 교체를
  검증 중. **텍스트-이미지는 관심 없음 — 이미지-이미지 유사도와 성능이 본제.**
- 실제 코퍼스는 **수천만 장** 규모 → 재임베딩 총비용(시간/전력)과 벡터 저장(Qdrant)
  이 의사결정의 핵심.
- 사용자가 확정한 방향: EG2 품질이 더 낫다는 주관적 판단은 이미 있음(48장 리포트).
  남은 것은 **정량 근거**.

## 2. 환경

### 로컬 박스 (deploy@... /home/deploy/git/viewer)
- git repo, 실험 스크립트: `scripts/eg2-probe/{probe,gallery,phases}.py`
- 로컬 venv: `.tmp/eg2probe/venv` (torch 2.14 cpu, transformers 5.19.0)
- 체크포인트: `.tmp/eg2probe/siglip2/`(model.safetensors 1500800904B,
  config.json, tokenizer.json), `.tmp/eg2probe/eg2/`(transformers 레이아웃,
  model.safetensors 1488915288B)
- 이전 실험 결과: `.tmp/eg2probe/sig.npz`, `.tmp/eg2probe/eg2.npz` (48장)

### Spark (wan@192.168.0.110, aarch64, DGX Spark GB10, CUDA 13.0)
- 모든 원격 명령은 `ssh -o BatchMode=yes wan@192.168.0.110 '<cmd>'`
- repo: `~/git/viewer` (origin = github.com/iwanhae/viewer)
- python: `~/git/viewer/worker/.venv` — `~/.local/bin/uv sync --extra cu130 --extra probe`
- 데이터: `~/git/viewer/.tmp/eg2probe/` 아래 `eg2/`(체크포인트), `siglip2/`,
  `photos/`(사진), `overnight/`(결과 전부)
- GPU는 vLLM(~47GB)·ComfyUI(~15GB)와 공유. EG2는 2~4GB면 충분. **GPU phase는
  직렬 실행**. 전력/온도 수치에는 타 워크로드 영향이 섞임(img/s가 1차 지표).

### 사진 소스
- `https://tmp.iwanhae.kr` = viewer 서버 배포본.
  - 피드: `/api/feed?mode=latest&limit=200&after=<nextCursor>` — **cursor는 응답의
    `nextCursor` 필드(불투명 문자열)이다. 사진 hash를 넣으면 조용히 1페이지로
    리셋되어 무한 루프(Phase 0에서 실제 발생, 수정 완료)**
  - 앨범 내부: `/api/albums/{albumId}` → `{albumId, photos: [{i, name, hash, w, h}]}`
  - 다운로드: `/api/image/{hash}?w=1024`
  - 피드는 앨범당 1장(커버)만 주므로, 클러스터링 약한 정답(albumId)을 얻으려면
    앨범 상세까지 호출해서 앨범 내부 사진을 수집해야 한다 (`cmd_fetch`가 그렇게 함)
  - 카탈로그 규모: 약 22,000 앨범
  - 읽기만 한다. 절대 쓰기 API 호출 금지.

## 3. 선행 결과 (반복 금지 — 이미 확인된 사실)

- 로컬 CPU(4코어): SigLIP2 0.24초/장(fp32), EG2 fp32 6.4초/장, bf16 23.5초/장
  (CPU bf16 에뮬레이션이 fp32보다 느림. **GPU에선 bf16이 정답**)
- EG2 프로세서 함정: `processor(images=flat_list)` → **샘플 1개로 합쳐서 풀링됨**.
  반드시 `images=[[img] for img in batch]` (샘플별 중첩 리스트)
- 비전 토큰 예산: `processor(..., max_soft_tokens=N)`, 범위 70~1120, 기본 280
- transformers 5.19.0이 `EmbeddingGemma2Model/Processor` 지원
- 48장 예비 실험: EG2 img-img 코사인 평균 0.679 vs SigLIP2 0.754(더 분별력 있음),
  한/영 쿼리 일치율 0.74 vs 0.50. mean pooling(fp32) + L2 정규화는 호출자 몫
- 제외 확정: 견고성(WebP/JPEG variant), 정밀도(bf16↔fp32), 텍스트 쿼리 스윕

## 4. Phase 목록 (직렬 실행, GPU 공유)

각 phase는 `phases.py <phase>` 서브커맨드로 실행. **멱등**: 결과 디렉터리에
`summary.json`이 있으면 스킵(`--force`로 재실행). 결과는
`~/git/viewer/.tmp/eg2probe/overnight/<phase>/`에.

| Phase | 명령 (Spark에서, repo 루트 기준) | 산출 | 검증 |
|---|---|---|---|
| 0 (진행중/완료) | git pull, uv sync, rsync eg2, curl siglip2, `fetch --n 1500` | env + 데이터 | subagent 리포트 |
| 0.5 | `fetch --n 20000` (지속 테스트 풀 확장) | photos.json 확장 | summary.photos |
| 1 | `calibrate` 후 `embed --budget 280 --batch <최적>` | 배치×스레드 매트릭스, 기준 벡터 eg2_280.npz | ms/img, VRAM, npz shape |
| 2 | `embed --budget {70,140,560,1120}` | 4개 npz + 스루풋 | 각 summary.img_per_s |
| 3 | `mrl --base 280` (CPU 전용, 2와 병렬 가능) | 512/256/128 kNN 일치율, pair corr | summary |
| 3.5 | `cluster` (CPU 전용, sklearn) | NMI/ARI vs albumId, medoid json | summary |
| 4 | `sustained --minutes 90 --pool 20000` (핵심) | samples.csv(전력/온도/클럭) + 지속 img/s, 10M/30M/50M 외삽 | summary |
| 5 | `siglip2` (같은 코퍼스 GPU fp32) | siglip2.npz + EG2 대비 kNN 일치율 | summary.vs_eg2 |
| 6 | (오케스트레이터 직접) 리포트 | report.md + gallery.html | 사용자 검수 |

- Phase 1 전에 Spark 로컬에서 작은 스모크 테스트 권장:
  `phases.py embed --budget 280 --subset 64 --out smoke` 성공 후 본 실행.
- Phase 4가 가장 오래 걸리고 실패 시 재시작 비용이 큼 → 90분 컨펌 전에 5분짜리
  `sustained --minutes 5` 선 점검.

## 5. 클러스터링 실험 (Phase 3.5 상세)

- 설정 그리드: 벡터 = {eg2_b280, eg2_b70, eg2_b1120, eg2_b280@256d, eg2_b280@128d,
  siglip2}, k = {50, 200, 1000}
- 지표: NMI/ARI vs albumId(약한 정답), 실루엣(5k 샘플), 클러스터 크기 분포
- 산출: k=200 구성의 medoid + 멤버 top5 json → 리포트 단계에서 갤러리 HTML
- 예상 결론 방향: 클러스터링은 검색보다 관대 → 70토큰+128d "절약 모드" 유효성 판단

## 6. 오케스트레이션 프로토콜

1. 각 Phase = `general` 서브에이전트 1개, **background** 실행, 완료 알림을 받고 다음
   Phase 투입 (GPU phase 직렬 보장).
2. 서브에이전트 프롬프트에는 반드시: ssh 접속법, 절대 경로, 실행할 명령, 검증 방법,
   "코드 수정 금지, 실패 시 원인만 보고" 포함. 멱등성 있으므로 재시도는 `--force` 없이.
3. 오케스트레이터는 summary.json만 읽어 진행 판단. 숫자가 이상하면(예: img/s 급락)
   먼저 nvidia-smi로 다른 워크로드 확인.
4. 최종 리포트: `~/git/viewer/.tmp/eg2probe/overnight/report.md` + `gallery.html`
   (kNN 이웃 40 앵커 × top5, 클러스터 medoid 갤러리 — 썸네일은
   `photos/` 상대경로 참조).

## 7. 리포트가 답해야 할 질문 (체크리스트)

1. GB10 지속 img/s (90분 기준, 워밍업 제외) → **3,000만장 재임베딩 = 며칠?**
2. SigLIP2 대비 재임베딩 비용 배수
3. 토큰 예산 최적점 (스루풋 × 이웃 일치율 트레이드오프 곡선)
4. MRL 최적 차원 (kNN 일치율 × Qdrant 메모리: 30M 벡터 기준
   768d≈92GB, 512d≈61GB, 256d≈31GB, 128d≈15GB fp32 + HNSW 그래프 오버헤드)
5. 클러스터링 품질 (NMI) — 절약 모드(70t/128d) 유효한가
6. 최종 권고: 교체 여부, 운영 설정(배치/예산/차원), 이관 전략(신규 사진부터? 전량?)

## 8. 리스크 메모

- Spark GPU의 vLLM이 메모리를 많이 쓰면 EG2 로딩이 OOM할 수 있음 → Phase 1 전
  nvidia-smi 확인, 실패 시 잠시 후 재시도.
- 피드 API는 공개 엔드포인트다. 요청 빈도를 8 스레드로 제한(코드에 고정), 서버에
  부하 주지 말 것.
- 사진 풀 2만 장은 전체 코퍼스의 일부 — "수천만장" 수치는 전부 외삽임을 리포트에 명시.
- catalog walk가 전체를 순회하면 몇 백만 페이지가 될 수 있으니 **2만 장 상한 유지**.

package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"revolcast-backend/internal/repository"
)

// ---- fakes ----

type fakeGameServerStore struct {
	created        *repository.GameServerRecord
	createErr      error
	createHash     string // 记录入库时收到的 api_secret_hash
	list           []*repository.GameServerRecord
	listErr        error
	listCalls      int    // 记录 ListGameServers 被调用次数（用于断言缓存命中）
	lastStatusFltr *int16 // 记录列表查询使用的 status 过滤条件
	byCode         *repository.GameServerRecord
	getErr         error
	bySecretHash   *repository.GameServerRecord
	secretHashErr  error
	lastSecretHash string // 记录按密钥哈希查询时收到的 api_secret_hash
	lastUpdate     repository.GameServerUpdate
	updated        *repository.GameServerRecord
	updateErr      error
	disabled       *repository.GameServerRecord
	disableErr     error
	rotated        *repository.GameServerRecord
	rotateHash     string // 记录轮换时收到的 api_secret_hash
	rotateErr      error
}

func (f *fakeGameServerStore) CreateGameServer(_ context.Context, code, name, description, apiSecretHash string) (*repository.GameServerRecord, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	rec := &repository.GameServerRecord{ID: 1, Code: code, Name: name, Status: 1}
	if description != "" {
		d := description
		rec.Description = &d
	}
	f.createHash = apiSecretHash
	return rec, nil
}

func (f *fakeGameServerStore) ListGameServers(_ context.Context, statusFilter *int16, _ int, _ int) ([]*repository.GameServerRecord, error) {
	f.listCalls++
	f.lastStatusFltr = statusFilter
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeGameServerStore) GetGameServerByCode(_ context.Context, _ string) (*repository.GameServerRecord, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byCode, nil
}

func (f *fakeGameServerStore) GetGameServerBySecretHash(_ context.Context, secretHash string) (*repository.GameServerRecord, error) {
	f.lastSecretHash = secretHash
	if f.secretHashErr != nil {
		return nil, f.secretHashErr
	}
	return f.bySecretHash, nil
}

func (f *fakeGameServerStore) UpdateGameServer(_ context.Context, _ string, u repository.GameServerUpdate) (*repository.GameServerRecord, error) {
	f.lastUpdate = u
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updated, nil
}

func (f *fakeGameServerStore) DisableGameServer(_ context.Context, _ string) (*repository.GameServerRecord, error) {
	if f.disableErr != nil {
		return nil, f.disableErr
	}
	return f.disabled, nil
}

func (f *fakeGameServerStore) RotateGameServerSecret(_ context.Context, _ string, newAPISecretHash string) (*repository.GameServerRecord, error) {
	f.rotateHash = newAPISecretHash
	if f.rotateErr != nil {
		return nil, f.rotateErr
	}
	return f.rotated, nil
}

type fakeServerRateLimiter struct {
	allowed bool
	err     error
}

func (f *fakeServerRateLimiter) AllowAttempt(_ context.Context, _ string, _ time.Duration, _ int64) (bool, error) {
	return f.allowed, f.err
}

type fakeServerSecretGenerator struct {
	raw  string
	hash string
	err  error
}

func (f fakeServerSecretGenerator) GenerateSecret() (string, string, error) {
	return f.raw, f.hash, f.err
}

// fakeServerCache 是内存版 ServerCache，用于断言命中/回填/失效行为。
type fakeServerCache struct {
	data   map[string][]byte
	getErr error
	setErr error
	delErr error
	gets   int
	sets   int
	dels   int
}

func newFakeServerCache() *fakeServerCache {
	return &fakeServerCache{data: map[string][]byte{}}
}

func (f *fakeServerCache) GetCache(_ context.Context, key string, dest any) (bool, error) {
	f.gets++
	if f.getErr != nil {
		return false, f.getErr
	}
	raw, ok := f.data[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeServerCache) SetCache(_ context.Context, key string, val any, _ time.Duration) error {
	f.sets++
	if f.setErr != nil {
		return f.setErr
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	f.data[key] = raw
	return nil
}

func (f *fakeServerCache) DelCache(_ context.Context, keys ...string) error {
	f.dels++
	if f.delErr != nil {
		return f.delErr
	}
	for _, k := range keys {
		delete(f.data, k)
	}
	return nil
}

// newGameServerService 组装测试实例，注入默认 fake。
func newGameServerService(store *fakeGameServerStore, rl *fakeServerRateLimiter) *GameServerService {
	svc := NewGameServerService(store, rl, newFakeServerCache(), GameServerConfig{})
	// 用可控密钥生成器替换默认（defaultServerSecretGenerator），便于断言。
	svc.secret = fakeServerSecretGenerator{raw: "raw-secret-plain", hash: "deadbeefhash"}
	return svc
}

// ---- Create ----

func TestGameServerCreate_Success(t *testing.T) {
	store := &fakeGameServerStore{created: &repository.GameServerRecord{ID: 1, Code: "survival-01", Name: "生存服", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	rec, secret, err := svc.Create(context.Background(), "1.2.3.4", "survival-01", "生存服", "desc")
	if err != nil {
		t.Fatalf("期望创建成功，实际 %v", err)
	}
	if secret != "raw-secret-plain" {
		t.Fatalf("期望返回一次性原始 secret，实际 %q", secret)
	}
	if store.createHash != "deadbeefhash" {
		t.Fatalf("期望入库哈希为生成器哈希，实际 %q", store.createHash)
	}
	if rec.Code != "survival-01" {
		t.Fatalf("期望返回记录 code，实际 %q", rec.Code)
	}
}

func TestGameServerCreate_CodeInvalid(t *testing.T) {
	svc := newGameServerService(&fakeGameServerStore{}, &fakeServerRateLimiter{allowed: true})
	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "Bad_Code", "name", ""); !errors.Is(err, ErrServerCodeInvalid) {
		t.Fatalf("期望 ErrServerCodeInvalid，实际 %v", err)
	}
}

func TestGameServerCreate_NameInvalid(t *testing.T) {
	svc := newGameServerService(&fakeGameServerStore{}, &fakeServerRateLimiter{allowed: true})
	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "ok-code", "", ""); !errors.Is(err, ErrServerNameInvalid) {
		t.Fatalf("期望 ErrServerNameInvalid，实际 %v", err)
	}
}

func TestGameServerCreate_RateLimited(t *testing.T) {
	svc := newGameServerService(&fakeGameServerStore{}, &fakeServerRateLimiter{allowed: false})
	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "ok-code", "name", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestGameServerCreate_CodeTaken(t *testing.T) {
	store := &fakeGameServerStore{createErr: repository.ErrServerCodeTaken}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "ok-code", "name", ""); !errors.Is(err, ErrServerCodeTaken) {
		t.Fatalf("期望 ErrServerCodeTaken，实际 %v", err)
	}
}

func TestGameServerCreate_SecretGenError(t *testing.T) {
	store := &fakeGameServerStore{}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	svc.secret = fakeServerSecretGenerator{err: errors.New("rand fail")}
	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "ok-code", "name", ""); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal（密钥生成失败），实际 %v", err)
	}
}

func TestGameServerCreate_StoreInternal(t *testing.T) {
	store := &fakeGameServerStore{createErr: errors.New("deadlock")}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "ok-code", "name", ""); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal（仓库未知错误），实际 %v", err)
	}
}

// ---- List / GetByCode ----

func TestGameServerList_Success(t *testing.T) {
	store := &fakeGameServerStore{list: []*repository.GameServerRecord{{ID: 1, Code: "a", Name: "A", Status: 1}}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	recs, err := svc.List(context.Background(), "1.2.3.4", nil, 20, 0)
	if err != nil {
		t.Fatalf("期望列表成功，实际 %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(recs))
	}
}

func TestGameServerList_RateLimited(t *testing.T) {
	svc := newGameServerService(&fakeGameServerStore{}, &fakeServerRateLimiter{allowed: false})
	if _, err := svc.List(context.Background(), "1.2.3.4", nil, 20, 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("期望 ErrRateLimited，实际 %v", err)
	}
}

func TestGameServerGetByCode_Success(t *testing.T) {
	store := &fakeGameServerStore{byCode: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.GetByCode(context.Background(), "1.2.3.4", "a"); err != nil {
		t.Fatalf("期望查询成功，实际 %v", err)
	}
}

func TestGameServerGetByCode_NotFound(t *testing.T) {
	store := &fakeGameServerStore{getErr: repository.ErrServerNotFound}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.GetByCode(context.Background(), "1.2.3.4", "missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("期望 ErrServerNotFound，实际 %v", err)
	}
}

// ---- Update ----

func TestGameServerUpdate_Success(t *testing.T) {
	store := &fakeGameServerStore{updated: &repository.GameServerRecord{ID: 1, Code: "a", Name: "新名", Status: 0}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	status := int16(0)
	name := "新名"
	if _, err := svc.Update(context.Background(), "1.2.3.4", "a", repository.GameServerUpdate{Name: &name, Status: &status}); err != nil {
		t.Fatalf("期望更新成功，实际 %v", err)
	}
	if store.lastUpdate.Name == nil || *store.lastUpdate.Name != "新名" {
		t.Fatalf("期望仓库收到 Name=新名，实际 %+v", store.lastUpdate)
	}
	if store.lastUpdate.Status == nil || *store.lastUpdate.Status != 0 {
		t.Fatalf("期望仓库收到 Status=0，实际 %+v", store.lastUpdate)
	}
}

func TestGameServerUpdate_NameInvalid(t *testing.T) {
	svc := newGameServerService(&fakeGameServerStore{}, &fakeServerRateLimiter{allowed: true})
	name := ""
	if _, err := svc.Update(context.Background(), "1.2.3.4", "a", repository.GameServerUpdate{Name: &name}); !errors.Is(err, ErrServerNameInvalid) {
		t.Fatalf("期望 ErrServerNameInvalid，实际 %v", err)
	}
}

func TestGameServerUpdate_StatusInvalid(t *testing.T) {
	svc := newGameServerService(&fakeGameServerStore{}, &fakeServerRateLimiter{allowed: true})
	status := int16(2) // 非法状态值
	if _, err := svc.Update(context.Background(), "1.2.3.4", "a", repository.GameServerUpdate{Status: &status}); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("期望 ErrInvalidParam（status 非法），实际 %v", err)
	}
}

func TestGameServerUpdate_NotFound(t *testing.T) {
	store := &fakeGameServerStore{updateErr: repository.ErrServerNotFound}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.Update(context.Background(), "1.2.3.4", "a", repository.GameServerUpdate{}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("期望 ErrServerNotFound，实际 %v", err)
	}
}

// ---- Disable ----

func TestGameServerDisable_Success(t *testing.T) {
	store := &fakeGameServerStore{disabled: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 0}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.Disable(context.Background(), "1.2.3.4", "a"); err != nil {
		t.Fatalf("期望禁用成功，实际 %v", err)
	}
}

func TestGameServerDisable_NotFound(t *testing.T) {
	store := &fakeGameServerStore{disableErr: repository.ErrServerNotFound}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.Disable(context.Background(), "1.2.3.4", "missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("期望 ErrServerNotFound，实际 %v", err)
	}
}

// ---- RotateSecret ----

func TestGameServerRotateSecret_Success(t *testing.T) {
	store := &fakeGameServerStore{rotated: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	secret, rec, err := svc.RotateSecret(context.Background(), "1.2.3.4", "a")
	if err != nil {
		t.Fatalf("期望轮换成功，实际 %v", err)
	}
	if secret != "raw-secret-plain" {
		t.Fatalf("期望返回一次性新 secret，实际 %q", secret)
	}
	if store.rotateHash != "deadbeefhash" {
		t.Fatalf("期望入库新哈希，实际 %q", store.rotateHash)
	}
	if rec == nil {
		t.Fatalf("期望返回更新后的记录")
	}
}

func TestGameServerRotateSecret_NotFound(t *testing.T) {
	store := &fakeGameServerStore{rotateErr: repository.ErrServerNotFound}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, _, err := svc.RotateSecret(context.Background(), "1.2.3.4", "missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("期望 ErrServerNotFound，实际 %v", err)
	}
}

// ---- ResolveServerBySecret（WS 接入鉴权）----

func TestResolveServerBySecret_Success(t *testing.T) {
	store := &fakeGameServerStore{bySecretHash: &repository.GameServerRecord{ID: 42, Code: "a", Name: "A", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})

	id, code, err := svc.ResolveServerBySecret(context.Background(), "raw-secret")
	if err != nil {
		t.Fatalf("期望鉴权成功，实际 %v", err)
	}
	if id != 42 {
		t.Fatalf("期望返回 game_servers.id=42，实际 %d", id)
	}
	// 连接标识需使用 server_code（Hub 按 server_code 管理游戏服务器连接）。
	if code != "a" {
		t.Fatalf("期望返回 server_code=a，实际 %q", code)
	}

	// 必须对下发的原始串求 SHA-256（hex），与 api_secret_hash 的生成口径一致。
	sum := sha256.Sum256([]byte("raw-secret"))
	if want := hex.EncodeToString(sum[:]); store.lastSecretHash != want {
		t.Fatalf("查询哈希应为 hex(SHA256(raw))=%q，实际 %q", want, store.lastSecretHash)
	}
}

func TestResolveServerBySecret_NotFoundOrDisabled(t *testing.T) {
	// secret 错误 / 服务器不存在 / 已停用（status=0）在仓库层统一为 ErrServerNotFound，biz 层不得区分。
	store := &fakeGameServerStore{secretHashErr: repository.ErrServerNotFound}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})

	if _, _, err := svc.ResolveServerBySecret(context.Background(), "wrong"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("期望 ErrServerNotFound，实际 %v", err)
	}
}

func TestResolveServerBySecret_StoreInternal(t *testing.T) {
	store := &fakeGameServerStore{secretHashErr: errors.New("pg: connection refused")}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})

	_, _, err := svc.ResolveServerBySecret(context.Background(), "raw-secret")
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	// 错误链须携带根因，便于内部日志定位。
	if !strings.Contains(err.Error(), "pg: connection refused") {
		t.Fatalf("期望错误链包含根因，实际 %v", err)
	}
}

// ---- ResolveSubscribableServer（Web WS 订阅校验）----

func TestResolveSubscribableServer_OK(t *testing.T) {
	store := &fakeGameServerStore{byCode: &repository.GameServerRecord{ID: 3, Code: "survival", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})

	rec, err := svc.ResolveSubscribableServer(context.Background(), "survival")
	if err != nil {
		t.Fatalf("期望校验通过，实际 %v", err)
	}
	if rec.ID != 3 || rec.Code != "survival" {
		t.Fatalf("返回记录不符: %+v", rec)
	}
}

func TestResolveSubscribableServer_InvalidCode(t *testing.T) {
	store := &fakeGameServerStore{byCode: &repository.GameServerRecord{ID: 3, Code: "survival", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})

	// 非法格式直接拒绝，不查库。
	if _, err := svc.ResolveSubscribableServer(context.Background(), "Bad_Code"); !errors.Is(err, ErrServerCodeInvalid) {
		t.Fatalf("期望 ErrServerCodeInvalid，实际 %v", err)
	}
}

func TestResolveSubscribableServer_NotFound(t *testing.T) {
	store := &fakeGameServerStore{getErr: repository.ErrServerNotFound}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.ResolveSubscribableServer(context.Background(), "missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("期望 ErrServerNotFound，实际 %v", err)
	}
}

// TestResolveSubscribableServer_Disabled 已停用服务器的订阅必须被拒绝（403 SERVER_DISABLED），
// 与「服务器不存在」（404）区分开，便于前端提示。
func TestResolveSubscribableServer_Disabled(t *testing.T) {
	store := &fakeGameServerStore{byCode: &repository.GameServerRecord{ID: 3, Code: "survival", Status: 0}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})

	_, err := svc.ResolveSubscribableServer(context.Background(), "survival")
	if !errors.Is(err, ErrServerDisabled) {
		t.Fatalf("期望 ErrServerDisabled，实际 %v", err)
	}
	if meta := ResolveErrorMeta(err); meta.HTTPStatus != 403 || meta.HTTPCode != 403 {
		t.Fatalf("停用服务器应映射为 403，实际 %+v", meta)
	}
}

func TestResolveSubscribableServer_StoreError(t *testing.T) {
	store := &fakeGameServerStore{getErr: errors.New("pg down")}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	if _, err := svc.ResolveSubscribableServer(context.Background(), "survival"); !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
}

// ---- ListAvailable（BIZ-008 可用服务器列表 + 缓存）----

// TestListAvailable_CacheMissThenHit 首次回源查库并回填缓存，第二次直接命中缓存（不再查库）。
func TestListAvailable_CacheMissThenHit(t *testing.T) {
	desc := "转生成魔法师拯救世界"
	store := &fakeGameServerStore{list: []*repository.GameServerRecord{
		{ID: 1, Code: "element", Name: "元素觉醒", Description: &desc, Status: 1},
		{ID: 2, Code: "afop", Name: "宁然一隅", Status: 1}, // Description=nil 应拍平为空串
	}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	cache := svc.cache.(*fakeServerCache)

	first, err := svc.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("期望查询成功，实际 %v", err)
	}
	if len(first) != 2 || first[0].Code != "element" || first[0].Description != desc {
		t.Fatalf("列表内容不符: %+v", first)
	}
	if first[1].Description != "" {
		t.Fatalf("NULL 简介应拍平为空串，实际 %q", first[1].Description)
	}
	if store.listCalls != 1 || cache.sets != 1 {
		t.Fatalf("首次应查库 1 次并写缓存 1 次，实际 list=%d set=%d", store.listCalls, cache.sets)
	}
	// 只允许 status=1（启用）的服务器进入「可用」列表。
	if store.lastStatusFltr == nil || *store.lastStatusFltr != 1 {
		t.Fatalf("期望按 status=1 过滤，实际 %v", store.lastStatusFltr)
	}

	second, err := svc.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("第二次期望命中缓存，实际 %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("缓存结果条数不符: %+v", second)
	}
	if store.listCalls != 1 {
		t.Fatalf("命中缓存后不应再查库，实际查库 %d 次", store.listCalls)
	}
}

// TestListAvailable_CacheFailureFailOpen Redis 故障时退化为直接查库，不阻断业务。
func TestListAvailable_CacheFailureFailOpen(t *testing.T) {
	store := &fakeGameServerStore{list: []*repository.GameServerRecord{{ID: 1, Code: "element", Name: "元素觉醒", Status: 1}}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	cache := svc.cache.(*fakeServerCache)
	cache.getErr = errors.New("redis down")
	cache.setErr = errors.New("redis down")

	list, err := svc.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("缓存故障应 fail-open，实际 %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("期望回源得到 1 条，实际 %d", len(list))
	}
}

// TestListAvailable_StoreError 查库失败映射为 ErrInternal（含根因）。
func TestListAvailable_StoreError(t *testing.T) {
	store := &fakeGameServerStore{listErr: errors.New("pg down")}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	_, err := svc.ListAvailable(context.Background())
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("期望 ErrInternal，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "pg down") {
		t.Fatalf("期望错误链包含根因，实际 %v", err)
	}
}

// TestListAvailable_Empty 无启用服务器时返回空切片（不是 nil，便于上层判断）。
func TestListAvailable_Empty(t *testing.T) {
	store := &fakeGameServerStore{list: []*repository.GameServerRecord{}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	list, err := svc.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("期望成功，实际 %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("期望空切片，实际 %#v", list)
	}
}

// ---- 缓存失效：Create / Update / Disable 成功后必须 DEL 列表缓存 ----

func TestGameServerCreate_InvalidatesCache(t *testing.T) {
	store := &fakeGameServerStore{created: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	cache := svc.cache.(*fakeServerCache)

	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "a", "A", ""); err != nil {
		t.Fatalf("期望创建成功，实际 %v", err)
	}
	if cache.dels != 1 {
		t.Fatalf("期望失效缓存 1 次，实际 %d", cache.dels)
	}
}

func TestGameServerUpdate_InvalidatesCache(t *testing.T) {
	store := &fakeGameServerStore{updated: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	cache := svc.cache.(*fakeServerCache)

	if _, err := svc.Update(context.Background(), "1.2.3.4", "a", repository.GameServerUpdate{}); err != nil {
		t.Fatalf("期望更新成功，实际 %v", err)
	}
	if cache.dels != 1 {
		t.Fatalf("期望失效缓存 1 次，实际 %d", cache.dels)
	}
}

func TestGameServerDisable_InvalidatesCache(t *testing.T) {
	store := &fakeGameServerStore{disabled: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 0}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	cache := svc.cache.(*fakeServerCache)

	if _, err := svc.Disable(context.Background(), "1.2.3.4", "a"); err != nil {
		t.Fatalf("期望禁用成功，实际 %v", err)
	}
	if cache.dels != 1 {
		t.Fatalf("期望失效缓存 1 次，实际 %d", cache.dels)
	}
}

// TestGameServerCreate_DelErrorIgnored 缓存失效失败不影响业务成功（由 TTL 兜底）。
func TestGameServerCreate_DelErrorIgnored(t *testing.T) {
	store := &fakeGameServerStore{created: &repository.GameServerRecord{ID: 1, Code: "a", Name: "A", Status: 1}}
	svc := newGameServerService(store, &fakeServerRateLimiter{allowed: true})
	svc.cache.(*fakeServerCache).delErr = errors.New("redis down")

	if _, _, err := svc.Create(context.Background(), "1.2.3.4", "a", "A", ""); err != nil {
		t.Fatalf("缓存失效失败不应影响创建结果，实际 %v", err)
	}
}

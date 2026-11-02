package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// catalogServer 造一个假上游：回 catalog，也回仓库的 tags/list。
//
// tags 是"仓库 → tag 列表"的映射；没列出的仓库回 404（上游没这个仓库）。
//
// ⚠️ 对账器会**先问上游"真的有镜像吗"**（tags/list 非空）才建行——
//
//	上游 catalog 会把"删过 manifest、只剩目录"的空壳仓库也列出来，
//	不问就会给用户建出"标签数 0"的幽灵仓库。
func catalogServer(t *testing.T, body string, tags map[string][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		// /v2/<name>/tags/list
		if strings.HasPrefix(r.URL.Path, "/v2/") && strings.HasSuffix(r.URL.Path, "/tags/list") {
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
			list, ok := tags[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			payload, _ := json.Marshal(map[string]any{"name": name, "tags": list})
			_, _ = w.Write(payload)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// catalogServerWithImages 造一个"catalog 里的每个仓库都有镜像"的假上游。
func catalogServerWithImages(t *testing.T, body string) *httptest.Server {
	t.Helper()
	var payload struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析 catalog: %v", err)
	}
	tags := make(map[string][]string, len(payload.Repositories))
	for _, name := range payload.Repositories {
		tags[name] = []string{"v1"}
	}
	return catalogServer(t, body, tags)
}

// seedProjectOnly 只建项目，不建仓库。
func seedProjectOnly(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	p, err := db.CreateProject(context.Background(), name, name, "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	return p.ID
}

// TestReconcilerAddsUpstreamRepo 覆盖"上游有、本地没有 → 建行"。
//
// 这是**目录空着**最常见的成因：上游 distribution 配了事件通知，
// 但回调没送达（或者压根没配），仓库行就永远建不出来。
func TestReconcilerAddsUpstreamRepo(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")

	up := catalogServerWithImages(t, `{"repositories":["team-a/svc"]}`)
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})

	added, removed, _, err := rec.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d，期望 1", added)
	}
	if removed != 0 {
		t.Fatalf("removed = %d，期望 0", removed)
	}

	project, err := db.ProjectByName(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	repo, err := db.RepositoryByPath(context.Background(), project.ID, "svc")
	if err != nil {
		t.Fatalf("仓库行没建出来: %v", err)
	}
	if repo.FullName != "team-a/svc" {
		t.Fatalf("仓库名 = %q", repo.FullName)
	}
	if repo.Visibility != domain.VisibilityPrivate {
		t.Fatalf("新建仓库可见性 = %q，期望 private", repo.Visibility)
	}
}

// TestReconcilerRepoVisibilityFollowsProject 覆盖"新仓库的可见性跟随项目"。
//
// 公开面要求"项目 public **且** 仓库 public"，
// 所以自动建仓时若一律写死 private，公开项目里的镜像**匿名拉不到**，
// 还得挨个手动改——与 `02-auth.md` §6「visibility 默认取项目内显式配置」不符。
func TestReconcilerRepoVisibilityFollowsProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// 公开项目。
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}

	up := catalogServerWithImages(t, `{"repositories":["pub-team/svc"]}`)
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})
	if _, _, _, err := rec.SyncOnce(ctx); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	repo, err := db.RepositoryByPath(ctx, project.ID, "svc")
	if err != nil {
		t.Fatalf("仓库行没建出来: %v", err)
	}
	if repo.Visibility != domain.VisibilityPublic {
		t.Fatalf("公开项目里新建仓库的可见性 = %q，期望 public（否则匿名拉不到）", repo.Visibility)
	}
}

// TestReconcilerEnqueuesRefreshWithUsableKey 覆盖"入队的键必须可用"。
//
// 刷新队列以仓库的 FullName 为键。建仓时若拿到的是半成品记录
// （FullName 为空），键就是空串 → 刷新被**静默跳过** →
// 目录页"有镜像但大小/标签全空"，而且没有任何日志。
func TestReconcilerEnqueuesRefreshWithUsableKey(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")

	up := catalogServerWithImages(t, `{"repositories":["team-a/svc"]}`)
	q := NewRefreshQueue()
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       q,
		Logger:      &fakeLogger{},
	})

	if _, _, _, err := rec.SyncOnce(context.Background()); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	key, ok := q.Take()
	if !ok {
		t.Fatalf("对账后队列里没有待刷新的仓库")
	}
	if key == "" {
		t.Fatalf("队列键是空串——刷新会被静默跳过")
	}
	if key != "team-a/svc" {
		t.Fatalf("队列键 = %q，期望 team-a/svc", key)
	}
}

// TestReconcilerSkipsRepoWithoutProject 覆盖"上游有但项目不存在 → 跳过"。
//
// 镜像名第一段就是项目名；项目不存在说明那是别的 registry 的仓库，
// 不能凭空造一个项目出来。
func TestReconcilerSkipsRepoWithoutProject(t *testing.T) {
	db := newTestDB(t)
	up := catalogServerWithImages(t, `{"repositories":["ghost/app"]}`)
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})

	added, _, _, err := rec.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d，期望 0（项目不存在不该建仓）", added)
	}
	if _, err := db.ProjectByName(context.Background(), "ghost"); err == nil {
		t.Fatalf("不该凭空建出项目 ghost")
	}
}

// TestReconcilerSkipsEmptyShellRepo 覆盖"上游只剩空壳仓库 → 不建行"。
//
// 这是"删掉的仓库重启后又冒出来、标签数 0"的根因：
// distribution 删掉 manifest 后**仓库目录还在**（还剩层链接），
// catalog 照样列它 → 对账器把它当成"上游有、本地没有"→ 建行。
//
// 判据是上游的 tags/list：空 tags = 一个镜像都没有的空壳。
func TestReconcilerSkipsEmptyShellRepo(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")

	// catalog 列了它，但 tags/list 是空的（空壳）。
	up := catalogServer(t, `{"repositories":["team-a/svc"]}`,
		map[string][]string{"team-a/svc": {}})
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})

	added, removed, _, err := rec.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d，期望 0（空壳仓库不该建行）", added)
	}
	if removed != 0 {
		t.Fatalf("removed = %d，期望 0", removed)
	}
	project, err := db.ProjectByName(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.RepositoryByPath(context.Background(), project.ID, "svc"); err == nil {
		t.Fatalf("不该给空壳仓库建出行（那会变成标签数 0 的幽灵仓库）")
	}
}

// TestReconcilerRemovesLocalRowForEmptyShellRepo 覆盖"本地行对应的上游只剩空壳 → 删行"。
//
// 自愈：用户删掉的仓库已经被对账建回来过（幽灵仓库），
// 下一轮对账要把它清掉。
func TestReconcilerRemovesLocalRowForEmptyShellRepo(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")
	project, err := db.ProjectByName(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.CreateRepository(context.Background(), project.ID, "svc", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	// 上游还列着它，但只剩空壳（0 标签）。
	up := catalogServer(t, `{"repositories":["team-a/svc"]}`,
		map[string][]string{"team-a/svc": {}})
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})

	added, removed, _, err := rec.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d，期望 0", added)
	}
	if removed != 1 {
		t.Fatalf("removed = %d，期望 1（上游只剩空壳，本地行该被清掉）", removed)
	}
	if _, err := db.RepositoryByPath(context.Background(), project.ID, "svc"); err == nil {
		t.Fatalf("幽灵仓库行还在")
	}
}

// TestReconcilerRemovesLocalOnlyRepo 覆盖"本地有、上游没有 → 删行"。
func TestReconcilerRemovesLocalOnlyRepo(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")

	up := catalogServerWithImages(t, `{"repositories":["team-a/svc"]}`)
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})
	// 先让上游有这个仓库，建出本地行。
	if _, _, _, err := rec.SyncOnce(context.Background()); err != nil {
		t.Fatalf("首次对账失败: %v", err)
	}

	// 现在上游只剩另一个仓库 → 本地的 team-a/svc 变成"本地有、上游没有" → 删。
	other := catalogServerWithImages(t, `{"repositories":["team-a/other"]}`)
	rec.upstreamURL = other.URL
	rec.upstream = other.Client()
	removed, _, _, err := rec.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d，期望 1", removed)
	}
}

// TestReconcilerKeepsLocalWhenUpstreamEmpty 覆盖"上游空 → 不删本地"。
//
// ⚠️ 这是**防误删**的闸门：上游刚重启/被打挂时 catalog 可能是空的，
// 那时全删等于把本地目录清空——不可逆的数据损失。
func TestReconcilerKeepsLocalWhenUpstreamEmpty(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")

	up := catalogServerWithImages(t, `{"repositories":["team-a/app"]}`)
	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})
	if _, _, _, err := rec.SyncOnce(context.Background()); err != nil {
		t.Fatalf("首次对账失败: %v", err)
	}

	// 上游变成空列表 → 不该删。
	empty := catalogServerWithImages(t, `{"repositories":[]}`)
	rec.upstreamURL = empty.URL
	rec.upstream = empty.Client()
	removed, _, _, err := rec.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d，期望 0（上游为空不得删本地目录）", removed)
	}
	project, _ := db.ProjectByName(context.Background(), "team-a")
	if _, err := db.RepositoryByPath(context.Background(), project.ID, "app"); err != nil {
		t.Fatalf("本地仓库行被误删: %v", err)
	}
}

// TestReconcilerFailsClosedOnUpstreamError 覆盖"上游不可达 → 什么都不做"。
func TestReconcilerFailsClosedOnUpstreamError(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()

	rec := NewReconciler(ReconcilerDeps{
		DB:          db,
		Upstream:    up.Client(),
		UpstreamURL: up.URL,
		Issuer:      newTestIssuer(t),
		Queue:       NewRefreshQueue(),
		Logger:      &fakeLogger{},
	})
	if _, _, _, err := rec.SyncOnce(context.Background()); err == nil {
		t.Fatalf("上游 500 时应当返回错误")
	}
}

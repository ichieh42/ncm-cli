package login

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/playwright-community/playwright-go"

	"ncm-cli/internal/config"
	"ncm-cli/internal/pwdriver"
)

type Options struct {
	ConfigDir string
	Headless  bool
	Timeout   time.Duration
	TracePath string
	Stdout    io.Writer
}

type accountResponse struct {
	Code    int `json:"code"`
	Profile *struct {
		UserID   int64  `json:"userId"`
		Nickname string `json:"nickname"`
	} `json:"profile"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
}

const (
	installPlaywrightDriverCommand  = "ncm driver install"
	installPlaywrightBrowserCommand = "ncm driver install --browser"
)

func Run(ctx context.Context, opts Options) (*config.UserInfo, error) {
	paths, err := config.Resolve(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	if err := config.EnsureDirs(paths); err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	pw, err := pwdriver.Run(opts.Stdout, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("启动 Playwright 失败，请先安装 driver：%s: %w", installPlaywrightDriverCommand, err)
	}
	defer pw.Stop()

	browserCtx, err := launchBrowserContext(pw, paths.ProfileDir, opts)
	if err != nil {
		return nil, err
	}
	defer browserCtx.Close()

	if opts.TracePath != "" {
		if err := browserCtx.Tracing().Start(playwright.TracingStartOptions{
			Screenshots: playwright.Bool(true),
			Snapshots:   playwright.Bool(true),
		}); err != nil {
			return nil, fmt.Errorf("启动 Playwright trace 失败: %w", err)
		}
		defer func() {
			if err := browserCtx.Tracing().Stop(opts.TracePath); err != nil {
				fmt.Fprintf(os.Stderr, "保存 Playwright trace 到 %s 失败: %v\n", opts.TracePath, err)
			} else {
				fmt.Fprintf(os.Stderr, "Playwright trace 已保存到 %s\n", opts.TracePath)
			}
		}()
	}

	pages := browserCtx.Pages()
	var page playwright.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = browserCtx.NewPage()
		if err != nil {
			return nil, err
		}
	}
	page.SetDefaultTimeout(30000)
	if _, err := page.Goto("https://music.163.com/#/discover", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(45000),
	}); err != nil {
		return nil, fmt.Errorf("打开网易云音乐失败: %w", err)
	}
	// 当前网易云网页版的主框架只是外壳，真正的 App（window.NEJ 与 GEnc 运行时）运行在 #g_iframe 里，
	// 因此需要先找到该 iframe，并在其上下文里等待页面运行时就绪、探测 ajax 调用函数。
	appFrame, ajaxKey, err := waitForAppRuntime(page)
	if err != nil {
		return nil, fmt.Errorf("等待网易云页面运行时失败: %w", err)
	}

	account, err := accountGet(appFrame, ajaxKey)
	if err != nil {
		return nil, err
	}
	if account.Profile == nil || account.Profile.UserID == 0 {
		fmt.Fprintln(opts.Stdout, "当前未登录，已打开登录窗口。请扫码或完成网页登录。")
		_, _ = page.Evaluate(`() => {
			if (typeof window.login === 'function') window.login();
			else if (window.top && typeof window.top.login === 'function') window.top.login();
		}`)
		account, err = waitForLogin(ctx, appFrame, ajaxKey, opts.Timeout, opts.Stdout)
		if err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintf(opts.Stdout, "已复用登录态：%s (%d)\n", account.Profile.Nickname, account.Profile.UserID)
	}

	if _, err := browserCtx.StorageState(paths.StorageStatePath); err != nil {
		return nil, fmt.Errorf("保存 Playwright 登录态失败: %w", err)
	}
	if err := config.HardenSessionFile(paths.StorageStatePath); err != nil {
		return nil, err
	}
	state, err := config.LoadStorageState(paths.StorageStatePath)
	if err != nil {
		return nil, err
	}
	info := config.UserInfo{
		ExportedAt:       time.Now().Format(time.RFC3339),
		UserID:           account.Profile.UserID,
		Nickname:         account.Profile.Nickname,
		CSRFPresent:      config.CSRF(state) != "",
		StorageStatePath: paths.StorageStatePath,
		SourceProfileDir: paths.ProfileDir,
	}
	if err := config.WriteUserInfo(paths.UserPath, info); err != nil {
		return nil, fmt.Errorf("写入用户信息失败: %w", err)
	}
	return &info, nil
}

func launchBrowserContext(pw *playwright.Playwright, profileDir string, opts Options) (playwright.BrowserContext, error) {
	baseOptions := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless:          playwright.Bool(opts.Headless),
		Viewport:          &playwright.Size{Width: 1280, Height: 900},
		Locale:            playwright.String("zh-CN"),
		TimezoneId:        playwright.String("Asia/Shanghai"),
		IgnoreHttpsErrors: playwright.Bool(true),
		Args:              []string{"--no-proxy-server"},
		UserAgent:         playwright.String("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
	}

	chromeOptions := baseOptions
	chromeOptions.Channel = playwright.String("chrome")
	browserCtx, chromeErr := pw.Chromium.LaunchPersistentContext(profileDir, chromeOptions)
	if chromeErr == nil {
		return browserCtx, nil
	}

	browserCtx, chromiumErr := pw.Chromium.LaunchPersistentContext(profileDir, baseOptions)
	if chromiumErr == nil {
		fmt.Fprintln(opts.Stdout, "未能启动系统 Chrome，已改用 Playwright Chromium。")
		return browserCtx, nil
	}

	return nil, fmt.Errorf("启动 Chrome 失败: %v。请安装 Chrome，或运行 %q 安装 Playwright Chromium；Playwright Chromium 启动也失败: %w", chromeErr, installPlaywrightBrowserCommand, chromiumErr)
}

// waitForAppRuntime 等待网易云 App 所在的 iframe 加载出 NEJ 运行时，并探测 ajax 调用函数。
// 主框架只是外壳，NEJ 运行时（window.NEJ 与 GEnc）位于 #g_iframe 内，且 ajax 模块成员名
// 是每次构建随机混淆的（历史上为 bc9T），因此需要动态探测。
func waitForAppRuntime(page playwright.Page) (playwright.Frame, string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range page.Frames() {
			if frame == page.MainFrame() || !isAppFrameURL(frame.URL()) {
				continue
			}
			key, err := detectAjaxKey(frame)
			if err != nil {
				continue // 框架正在加载或导航，稍后重试
			}
			if key != "" {
				return frame, key, nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, "", fmt.Errorf("等待页面运行时超时（未在网易云 iframe 中找到 NEJ ajax 运行时）")
}

// detectAjaxKey 在网易云 App 框架内探测真正的 ajax 请求函数成员名。
// 网易云构建产物中该成员名随机混淆，只能逐个调用 2 参数候选函数请求只读账号接口，
// 返回数字 code 的即为 ajax 函数。
func detectAjaxKey(frame playwright.Frame) (string, error) {
	value, err := frame.Evaluate(`() => new Promise((resolve) => {
		const m = window.NEJ?.P?.('nej.j');
		if (!m || window.GEnc !== true) return resolve({ key: '' });
		const keys = Object.keys(m).filter(k => typeof m[k] === 'function' && m[k].length === 2);
		let idx = 0;
		let settled = false;
		const finish = (r) => { if (!settled) { settled = true; resolve(r); } };
		const next = () => {
			if (idx >= keys.length) return finish({ key: '' });
			const k = keys[idx++];
			let called = false;
			try {
				m[k]('/api/w/nuser/account/get', {
					type: 'json',
					method: 'post',
					onload: (res) => {
						called = true;
						if (res && typeof res.code === 'number') finish({ key: k });
						else next();
					},
					onerror: () => { called = true; next(); },
				});
			} catch (e) { called = true; }
			setTimeout(() => { if (!called && !settled) next(); }, 800);
		};
		next();
	})`)
	if err != nil {
		return "", err
	}
	if m, ok := value.(map[string]any); ok {
		if key, ok := m["key"].(string); ok && key != "" {
			return key, nil
		}
	}
	return "", nil
}

func isAppFrameURL(raw string) bool {
	if raw == "" || raw == "about:blank" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Host == "music.163.com"
}

func accountGet(frame playwright.Frame, ajaxKey string) (*accountResponse, error) {
	value, err := frame.Evaluate(fmt.Sprintf(`() => new Promise((resolve) => {
		const ajax = window.NEJ?.P?.('nej.j')?.[%s];
		if (!ajax) return resolve({ code: -1, message: 'NEJ ajax unavailable' });
		ajax('/api/w/nuser/account/get', {
			type: 'json',
			method: 'post',
			onload: (res) => resolve(res),
			onerror: (err) => resolve({ code: err?.code || -1, message: err?.message || err?.msg || 'error' }),
		});
	})`, strconv.Quote(ajaxKey)))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out accountResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func waitForLogin(ctx context.Context, frame playwright.Frame, ajaxKey string, timeout time.Duration, stdout io.Writer) (*accountResponse, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var latest *accountResponse
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			msg := ""
			if latest != nil {
				msg = latest.Message
				if msg == "" {
					msg = latest.Msg
				}
			}
			return nil, fmt.Errorf("等待登录超时，最后账号状态：%s", msg)
		case <-ticker.C:
			account, err := accountGet(frame, ajaxKey)
			if err != nil {
				return nil, err
			}
			latest = account
			if account.Profile != nil && account.Profile.UserID != 0 {
				fmt.Fprintln(stdout)
				return account, nil
			}
			fmt.Fprint(stdout, ".")
		}
	}
}

func init() {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		_ = os.Unsetenv(key)
	}
}

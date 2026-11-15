package console

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

const assistantMarkdownFixture = "## 网关状态\n\n**已恢复**，可以继续调用。\n\n| 时间 | 模型 | 问题 |\n|---|---|---|\n| 10:58 | glm-model | DNS 解析超时 |\n| 11:01 | deepseek-model | 请求已取消 |\n\n### 建议\n\n1. 检查本机 DNS。\n2. 重试一个简短请求。\n\n```sh\ncurl http://localhost:4001/healthz\n```\n\n[文档](https://example.com/docs) · [非法链接](javascript:alert(1))"

func TestMarkdownLinksUseSafeNativeActions(t *testing.T) {
	var document markdownDocument
	tt := ui.NewTester(func(c *ui.Context) {
		document.render(c, assistantMarkdownFixture)
	}, 600, 400)
	click(t, tt, "文档")
	if urls := tt.OpenedURLs(); len(urls) != 1 || urls[0] != "https://example.com/docs" {
		t.Fatal("native link did not open", urls)
	}
	click(t, tt, "非法链接")
	if len(tt.OpenedURLs()) != 1 {
		t.Fatal("executable model-generated URL opened")
	}
}

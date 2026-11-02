//go:build windows

package tray

import (
	"strings"
	"testing"
)

// 菜单结构只能靠人手点，写错了自动验收很难发现，这里锁住关键约束：
// 有「推送当前剪贴板」，且不再有启用/停用同步（同步开关只在设置页）。
func TestMenuItems(t *testing.T) {
	items := menuItems()
	if len(items) != 4 {
		t.Fatalf("菜单项数量应为 4，实际 %d", len(items))
	}

	var push, quit *menuItem
	for i := range items {
		it := &items[i]
		if strings.Contains(it.text, "同步") {
			t.Fatalf("托盘菜单不应再出现同步开关：%q", it.text)
		}
		switch it.id {
		case cmdPush:
			push = it
		case cmdQuit:
			quit = it
		}
	}

	if push == nil || push.text != "推送当前剪贴板" {
		t.Fatalf("缺少「推送当前剪贴板」菜单项: %+v", push)
	}
	if quit == nil || quit.text != "退出" {
		t.Fatalf("缺少「退出」菜单项: %+v", quit)
	}
	if items[0].text != "打开设置" || items[0].flags&mfDefault == 0 {
		t.Fatalf("第一项应为默认的「打开设置」: %+v", items[0])
	}
}

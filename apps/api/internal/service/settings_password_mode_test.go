package service

import (
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestPasswordModeBuiltInDefaultMatchesSettingDef 锁定「两层默认值必须一致」。
//
// 设置项定义里的 Default 只喂给**设置页显示**（"默认值：optional"），
// 运行期生效值来自内置默认值 config.Default（config.go）。
//
// ⚠️ 这两层一旦不一致，页面就会骗人：
// 显示"默认值 optional"、实际生效 optional 之外的另一个值，
// 而用户看到的请求体就是那个值的效果（明文过网）。
func TestPasswordModeBuiltInDefaultMatchesSettingDef(t *testing.T) {
	def, ok := defByKey("password.mode")
	if !ok {
		t.Fatal("没有 password.mode 设置项")
	}
	builtIn := config.Default().Password.Mode
	if def.Default != builtIn {
		t.Fatalf(
			"password.mode 的内置默认值（config.Default = %q）与设置项定义（%q = %q）不一致",
			builtIn, def.Key, def.Default,
		)
	}
	if builtIn != "optional" {
		t.Fatalf("内置默认值 = %q, 期望 %q（默认应当是前后端都发密文）", builtIn, "optional")
	}
}

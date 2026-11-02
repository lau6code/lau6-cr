package objectstore

import (
	"errors"
	"strings"
	"testing"
)

// TestSplitEndpoint 覆盖 endpoint 的拆解。
//
// minio-go 要的是 host[:port]（不带 scheme），
// distribution 的 `regionendpoint` 要的是带 scheme 的完整 URL——
// 两边要求相反，所以由本函数统一拆一次。
func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		secure   bool
		wantHost string
		wantTLS  bool
		wantErr  bool
	}{
		{name: "https", endpoint: "https://minio.example.com", wantHost: "minio.example.com", wantTLS: true},
		{name: "http", endpoint: "http://minio.example.com", wantHost: "minio.example.com", wantTLS: false},
		{name: "带端口", endpoint: "https://minio.example.com:9000", wantHost: "minio.example.com:9000", wantTLS: true},
		{name: "无 scheme 用 secure", endpoint: "minio.example.com:9000", secure: true, wantHost: "minio.example.com:9000", wantTLS: true},
		{name: "无 scheme 不用 secure", endpoint: "minio.example.com:9000", wantHost: "minio.example.com:9000", wantTLS: false},
		{name: "带路径", endpoint: "https://minio.example.com/bucket", wantErr: true},
		{name: "协议不对", endpoint: "ftp://minio.example.com", wantErr: true},
		{name: "没有主机名", endpoint: "https://", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, tls, err := splitEndpoint(tc.endpoint, tc.secure)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q，期望 %q", host, tc.wantHost)
			}
			if tls != tc.wantTLS {
				t.Errorf("tls = %v，期望 %v", tls, tc.wantTLS)
			}
		})
	}
}

// TestSecureFromEndpoint 覆盖"secure 由 endpoint 的 scheme 推导"。
//
// 让用户把协议和 secure 开关填成两处可能打架的值，
// 就会出现"探测用 http、distribution 用 https"的分裂。
func TestSecureFromEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		fallback bool
		want     bool
	}{
		{endpoint: "https://minio.example.com", fallback: false, want: true},
		{endpoint: "http://minio.example.com", fallback: true, want: false},
		{endpoint: "minio.example.com", fallback: true, want: true},
		{endpoint: "minio.example.com", fallback: false, want: false},
	}
	for _, tc := range cases {
		if got := SecureFromEndpoint(tc.endpoint, tc.fallback); got != tc.want {
			t.Errorf("SecureFromEndpoint(%q, %v) = %v，期望 %v", tc.endpoint, tc.fallback, got, tc.want)
		}
	}
}

// TestScrubError 覆盖"错误信息里不能漏出凭据"。
//
// 错误会进日志、进向导提示框、进 setup 响应体——密钥不能从那里漏出去。
func TestScrubError(t *testing.T) {
	cfg := S3Config{AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t"}
	err := scrubError(errors.New("请求失败：AKIAEXAMPLE / s3cr3t 无权限"), cfg)
	msg := err.Error()
	if strings.Contains(msg, "AKIAEXAMPLE") || strings.Contains(msg, "s3cr3t") {
		t.Fatalf("错误信息里漏出了凭据: %s", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Errorf("应当把凭据替换成 ***: %s", msg)
	}
}

// TestProbeRejectsIncompleteConfig 覆盖"参数不全时不发起连接"。
func TestProbeRejectsIncompleteConfig(t *testing.T) {
	if _, err := ProbeS3(t.Context(), S3Config{}); err == nil {
		t.Fatal("缺 endpoint 时应当报错")
	}
	if _, err := ProbeS3(t.Context(), S3Config{Endpoint: "https://minio.example.com"}); err == nil {
		t.Fatal("缺 bucket 时应当报错")
	}
}

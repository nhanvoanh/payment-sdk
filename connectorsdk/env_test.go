package connectorsdk

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/tlstest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestOptionsFromEnv(t *testing.T) {
	f := tlstest.New(t).Server("srv.test")
	full := map[string]string{
		"CONNECTOR_TLS_CERT": f.CertFile, "CONNECTOR_TLS_KEY": f.KeyFile, "CONNECTOR_TLS_CA": f.CAFile,
		"CONNECTOR_ALLOWED_CLIENTS": " core.test , ,other.test",
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(m, kv[i])
			} else {
				m[kv[i]] = kv[i+1]
			}
		}
		return m
	}

	t.Run("mTLS đầy đủ", func(t *testing.T) {
		o, err := OptionsFromEnv(env(full))
		require.NoError(t, err)
		require.Equal(t, ":9090", o.ListenAddr)
		require.False(t, o.Insecure)
		require.NotNil(t, o.TLS)
		require.Equal(t, tls.RequireAndVerifyClientCert, o.TLS.ClientAuth)
		require.Equal(t, []string{"core.test", "other.test"}, o.AllowedClients)
	})
	t.Run("địa chỉ nghe", func(t *testing.T) {
		o, err := OptionsFromEnv(env(with("CONNECTOR_LISTEN_ADDR", "127.0.0.1:7000")))
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:7000", o.ListenAddr)
	})
	t.Run("insecure rõ ràng", func(t *testing.T) {
		o, err := OptionsFromEnv(env(map[string]string{"CONNECTOR_INSECURE": "true"}))
		require.NoError(t, err)
		require.True(t, o.Insecure)
		require.Nil(t, o.TLS)
	})
	for name, m := range map[string]map[string]string{
		"không có gì":              {},
		"INSECURE=1 không bật":     {"CONNECTOR_INSECURE": "1"},
		"INSECURE=TRUE không bật":  {"CONNECTOR_INSECURE": "TRUE"},
		"thiếu cert":               with("CONNECTOR_TLS_CERT", ""),
		"thiếu key":                with("CONNECTOR_TLS_KEY", ""),
		"thiếu CA":                 with("CONNECTOR_TLS_CA", ""),
		"file không tồn tại":       with("CONNECTOR_TLS_CA", "/không/có"),
		"insecure cùng cert":       with("CONNECTOR_INSECURE", "true"),
		"không có allowed clients": with("CONNECTOR_ALLOWED_CLIENTS", ""),
	} {
		t.Run("lỗi: "+name, func(t *testing.T) {
			_, err := OptionsFromEnv(env(m))
			require.Error(t, err)
		})
	}
}

func TestOptionsFromEnv_MaxConcurrentStreams(t *testing.T) {
	base := map[string]string{"CONNECTOR_INSECURE": "true"}
	with := func(v string) func(string) string {
		m := map[string]string{"CONNECTOR_MAX_CONCURRENT_STREAMS": v}
		for k, x := range base {
			m[k] = x
		}
		return env(m)
	}
	o, err := OptionsFromEnv(env(base))
	require.NoError(t, err)
	require.Equal(t, uint32(1000), o.MaxConcurrentStreams, "mặc định 1000")
	for v, want := range map[string]uint32{"100": 100, "5000": 5000, "100000": 100000} {
		o, err := OptionsFromEnv(with(v))
		require.NoError(t, err, v)
		require.Equal(t, want, o.MaxConcurrentStreams, v)
	}
	for _, v := range []string{"99", "100001", "0", "-1", "abc", "1e3", " 200"} {
		_, err := OptionsFromEnv(with(v))
		require.ErrorContains(t, err, "CONNECTOR_MAX_CONCURRENT_STREAMS", v)
	}
}

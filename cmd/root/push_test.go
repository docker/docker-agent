package root

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/protect"
)

func writeKey(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return protect.FilePrefix + path
}

func TestResolveKey_Env(t *testing.T) {
	secretFile := writeKey(t, "secret", []byte("a symmetric secret long enough"))

	t.Run("unset", func(t *testing.T) {
		t.Setenv(envEncryptKey, "")
		key, err := resolveKey("")
		require.NoError(t, err)
		assert.Nil(t, key)
	})

	t.Run("inline env", func(t *testing.T) {
		t.Setenv(envEncryptKey, "an env secret that is long enough")
		key, err := resolveKey("")
		require.NoError(t, err)
		assert.True(t, key.Symmetric())
	})

	t.Run("file env", func(t *testing.T) {
		t.Setenv(envEncryptKey, secretFile)
		key, err := resolveKey("")
		require.NoError(t, err)
		assert.True(t, key.Symmetric())
	})

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(envEncryptKey, "an env secret that is long enough")
		fromFlag, err := resolveKey("a flag secret that is long enough")
		require.NoError(t, err)
		fromEnv, err := protect.ParseKey([]byte("an env secret that is long enough"))
		require.NoError(t, err)
		assert.NotEqual(t, fromEnv.Identity(), fromFlag.Identity())
	})

	t.Run("invalid env", func(t *testing.T) {
		t.Setenv(envEncryptKey, "short")
		_, err := resolveKey("")
		require.ErrorIs(t, err, protect.ErrSecretTooShort)
	})
}

func TestPushFlags_Protection(t *testing.T) {
	t.Parallel()

	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privDER, err := x509.MarshalPKCS8PrivateKey(edPriv)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(edPub)
	require.NoError(t, err)

	secret := writeKey(t, "secret", []byte("a symmetric secret long enough\n"))
	short := writeKey(t, "short", []byte("short"))
	edPrivPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	edPrivFile := writeKey(t, "ed25519", edPrivPEM)
	edPubFile := writeKey(t, "ed25519.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))

	tests := []struct {
		name    string
		flags   pushFlags
		wantOpt bool
		wantErr error
		errMsg  string
	}{
		{name: "no key", flags: pushFlags{}},
		{name: "encrypt without key", flags: pushFlags{encrypt: true}, errMsg: "--encrypt requires --key"},
		{name: "missing key file", flags: pushFlags{key: protect.FilePrefix + filepath.Join(t.TempDir(), "nope")}, errMsg: "reading key file"},
		{name: "short secret", flags: pushFlags{key: short}, wantErr: protect.ErrSecretTooShort},
		{name: "secret sign", flags: pushFlags{key: secret}, wantOpt: true},
		{name: "secret encrypt", flags: pushFlags{key: secret, encrypt: true}, wantOpt: true},
		{name: "inline secret", flags: pushFlags{key: "an inline secret long enough"}, wantOpt: true},
		{name: "inline short secret", flags: pushFlags{key: "short"}, wantErr: protect.ErrSecretTooShort},
		{name: "inline pem key", flags: pushFlags{key: string(edPrivPEM)}, wantOpt: true},
		{name: "ed25519 sign", flags: pushFlags{key: edPrivFile}, wantOpt: true},
		{name: "ed25519 cannot encrypt", flags: pushFlags{key: edPrivFile, encrypt: true}, wantErr: protect.ErrCannotEncrypt},
		{name: "public key cannot sign", flags: pushFlags{key: edPubFile}, wantErr: protect.ErrCannotSign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt, err := tt.flags.protection()
			switch {
			case tt.wantErr != nil:
				require.ErrorIs(t, err, tt.wantErr)
			case tt.errMsg != "":
				require.ErrorContains(t, err, tt.errMsg)
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantOpt, opt != nil)
		})
	}
}

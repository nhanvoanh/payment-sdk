package paykit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMoney(t *testing.T) {
	_, err := NewMoney(-1, "VND")
	assert.ErrorIs(t, err, ErrNegativeAmount)

	for _, cur := range []string{"vnd", "VN", "", "VNDD", "V1D"} {
		_, err := NewMoney(1, cur)
		assert.ErrorIs(t, err, ErrInvalidCurrency, cur)
	}

	m, err := NewMoney(150000, "VND")
	require.NoError(t, err)
	assert.Equal(t, int64(150000), m.Amount)
	assert.Equal(t, "VND", m.Currency)
	assert.False(t, m.IsZero())
	assert.Equal(t, "150000 VND", m.String())

	z, err := NewMoney(0, "VND")
	require.NoError(t, err)
	assert.True(t, z.IsZero())
}

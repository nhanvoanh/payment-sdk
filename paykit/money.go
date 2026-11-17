// Package paykit chứa từ vựng dùng chung giữa core và connector của đối tác.
package paykit

import (
	"errors"
	"strconv"
)

var (
	ErrNegativeAmount  = errors.New("paykit: số tiền không được âm")
	ErrInvalidCurrency = errors.New("paykit: tiền tệ phải là 3 chữ hoa")
)

// Money là số tiền nguyên theo đơn vị nhỏ nhất của tiền tệ; Currency luôn 3 chữ hoa.
type Money struct {
	Amount   int64
	Currency string
}

// NewMoney kiểm tra bất biến của Money: không âm và tiền tệ 3 chữ hoa.
func NewMoney(amount int64, currency string) (Money, error) {
	if amount < 0 {
		return Money{}, ErrNegativeAmount
	}
	if len(currency) != 3 {
		return Money{}, ErrInvalidCurrency
	}
	for i := 0; i < 3; i++ {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return Money{}, ErrInvalidCurrency
		}
	}
	return Money{Amount: amount, Currency: currency}, nil
}

func (m Money) IsZero() bool { return m.Amount == 0 }

func (m Money) String() string { return strconv.FormatInt(m.Amount, 10) + " " + m.Currency }

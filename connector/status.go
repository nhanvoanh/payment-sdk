// Package connector định nghĩa contract mà mỗi connector đối tác phải theo.
package connector

// Status là trạng thái do đối tác báo về, đã chuẩn hoá về từ vựng chung.
type Status string

const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
	StatusOnHold    Status = "on_hold"
)

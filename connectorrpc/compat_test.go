package connectorrpc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// addedFields là các trường thêm sau bản đầu của contract: số trường là hợp đồng trên dây, không bao giờ đổi
// hay tái dùng. Bên chưa biết trường (connector hay core cũ) bỏ qua nó.
var addedFields = []struct {
	message string
	field   string
	number  protoreflect.FieldNumber
}{
	{"Customer", "address", 5},
}

func TestProto_AddedFieldNumbers(t *testing.T) {
	fd := connectorpb.File_hasaki_payment_connector_v1_connector_proto
	for _, a := range addedFields {
		md := fd.Messages().ByName(protoreflect.Name(a.message))
		require.NotNil(t, md, a.message)
		f := md.Fields().ByName(protoreflect.Name(a.field))
		require.NotNil(t, f, a.message+"."+a.field)
		assert.Equal(t, a.number, f.Number(), "%s.%s", a.message, a.field)
	}
}

// oldSchema dựng lại file proto như bản cũ (bỏ các trường thêm sau) để giả lập bên chưa biết trường mới.
func oldSchema(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	fdp := protodesc.ToFileDescriptorProto(connectorpb.File_hasaki_payment_connector_v1_connector_proto)
	drop := map[string]map[string]bool{}
	for _, a := range addedFields {
		if drop[a.message] == nil {
			drop[a.message] = map[string]bool{}
		}
		drop[a.message][a.field] = true
	}
	for _, m := range fdp.MessageType {
		var keep []*descriptorpb.FieldDescriptorProto
		for _, f := range m.Field {
			if !drop[m.GetName()][f.GetName()] {
				keep = append(keep, f)
			}
		}
		m.Field = keep
	}
	// Sổ đăng ký riêng (không phải sổ toàn cục) nên giữ nguyên tên file và package.
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(timestampFile()), fdp}})
	require.NoError(t, err)
	d, err := files.FindFileByPath(fdp.GetName())
	require.NoError(t, err)
	return d
}

func timestampFile() protoreflect.FileDescriptor {
	return (&connectorpb.CreatePaymentResponse{}).ProtoReflect().Descriptor().Fields().ByName("expires_at").Message().ParentFile()
}

// Bên cũ đọc thông điệp mới: trường mới bị bỏ qua, các trường cũ còn nguyên; bên cũ gửi thì trường mới rỗng.
func TestProto_OldPeerIgnoresAddedFields(t *testing.T) {
	old := oldSchema(t)
	in := connector.CreatePaymentRequest{TransactionCode: "TX1", IdempotencyKey: "idem", Money: paykit.Money{Amount: 1, Currency: "VND"},
		Method: paykit.MethodQR, Customer: cust}
	b, err := proto.Marshal(connectorrpc.CreatePaymentRequestToProto(in))
	require.NoError(t, err)

	md := old.Messages().ByName("CreatePaymentRequest")
	msg := dynamicpb.NewMessage(md)
	require.NoError(t, proto.Unmarshal(b, msg))
	assert.Equal(t, "TX1", msg.Get(md.Fields().ByName("transaction_code")).String())
	c := msg.Get(md.Fields().ByName("customer")).Message()
	assert.Equal(t, cust.Name, c.Get(c.Descriptor().Fields().ByName("name")).String())
	assert.Nil(t, c.Descriptor().Fields().ByName("address"), "lược đồ cũ không có address")

	// Bên cũ gửi lại (bỏ phần lạ như một bản cài cũ không giữ trường lạ): bên mới đọc ra địa chỉ rỗng.
	c.SetUnknown(nil)
	back, err := proto.Marshal(msg)
	require.NoError(t, err)
	var got connectorpb.CreatePaymentRequest
	require.NoError(t, proto.Unmarshal(back, &got))
	out := connectorrpc.CreatePaymentRequestFromProto(&got)
	assert.Empty(t, out.Customer.Address)
	assert.Equal(t, cust.Name, out.Customer.Name)
}

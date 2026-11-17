package table

import (
	"testing"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"

	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
)

func TestPrimaryChassisMonitorCondition(t *testing.T) {
	cm, err := ovnsb.FullDatabaseModel()
	require.NoError(t, err)
	dm, errs := model.NewDatabaseModel(ovnsb.Schema(), cm)
	require.Empty(t, errs)
	binding := &ovnsb.PortBinding{}
	info, err := dm.NewModelInfo(binding)
	require.NoError(t, err)
	chassis := "11111111-1111-1111-1111-111111111111"
	condition, err := dm.Mapper.NewCondition(info, &binding.Chassis, ovsdb.ConditionEqual, &chassis)
	require.NoError(t, err)
	require.Equal(t, "chassis", condition.Column)
	require.Equal(t, ovsdb.ConditionEqual, condition.Function)
	require.Equal(t, ovsdb.OvsSet{GoSet: []any{ovsdb.UUID{GoUUID: chassis}}}, condition.Value)
	// Primary UUID matching must not be silently replaced by additional_chassis
	// membership, which would advertise a prepared migration target too early.
	config := &monitorConfig{}
	require.NoError(t, WithConditionalTable(binding, model.Condition{Field: &binding.Chassis, Function: ovsdb.ConditionEqual, Value: &chassis})(config))
	require.Len(t, config.conditional, 1)
	require.Empty(t, config.tables)
	require.Error(t, WithConditionalTable(binding)(config))
}

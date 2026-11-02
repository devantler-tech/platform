package main

import "github.com/devantler-tech/platform/internal/longhornmigration"

const (
	migrationPoolLabel      = longhornmigration.PoolLabel
	migrationEnabledLabel   = longhornmigration.EnabledLabel
	capacityOwnerAnnotation = longhornmigration.OwnerAnnotation
	capacityOwnerValue      = longhornmigration.OwnerValue
	scaleDownDisabled       = longhornmigration.ScaleDownDisabled
)

type migrationPool struct {
	name, role, serverType string
	count                  int
}

func configuredMigrationPools(data []byte) (map[string]migrationPool, error) {
	pools, err := longhornmigration.ConfiguredPools(data)
	if err != nil {
		return nil, err
	}
	result := map[string]migrationPool{}
	for role, p := range pools {
		result[role] = migrationPool{name: p.Name, role: p.Role, serverType: p.ServerType, count: p.Count}
	}
	return result, nil
}

func verifyMigrationNode(n object, p migrationPool) error {
	return longhornmigration.VerifyNode(n, longhornmigration.Pool{Name: p.name, Role: p.role, ServerType: p.serverType, Count: p.count})
}

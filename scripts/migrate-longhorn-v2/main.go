// Command migrate-longhorn-v2 prepares Longhorn V2 workers.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// main admits explicit migration operations through their distinct confirmations
// and protected workflow identities before constructing any production client.
func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "Migration operations accept no command-line arguments.")
		os.Exit(1)
	}
	operation := os.Getenv("LONGHORN_V2_OPERATION")
	if operation == runtimePublicationCaptureOperation || operation == runtimePublicationCheckOperation {
		if err := runRuntimePublication(context.Background(), os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	target := os.Getenv("LONGHORN_V2_NODE")
	var detachedVolume string
	var authorization error
	switch operation {
	case "prepare-worker":
		authorization = authorize(os.Getenv, target)
	case "prepare-unused-temporary-worker":
		authorization = authorizeTemporaryPreparation(os.Getenv, target)
	case "prepare-retained-temporary-worker":
		authorization = authorizeRetainedTemporaryPreparation(os.Getenv, target)
	case observeTemporaryRecoveryOperation:
		authorization = authorizeTemporaryRecoveryObservation(os.Getenv, target)
	case temporaryReserveObservation:
		authorization = authorizeTemporaryReserveObservation(os.Getenv)
	case "benchmark-v1", "benchmark-v2":
		authorization = authorizeBenchmark(os.Getenv, operation[len("benchmark-"):])
	case recoverUnstartedV1Operation:
		authorization = authorizeUnstartedV1Recovery(os.Getenv)
	case recoverBoundV1Operation:
		authorization = authorizeBoundV1Recovery(os.Getenv)
	case "register-temporary-v2-storage":
		authorization = authorizeTemporaryActivation(os.Getenv)
	case nativeSeedOperation:
		authorization = authorizeNativeSeed(os.Getenv)
	case nativeRestoreOperation:
		authorization = authorizeNativeRestore(os.Getenv)
	case nativeRestoreResumeOperation:
		_, authorization = nativeRestoreResumeRun(os.Getenv)
	case nativeReattachmentOperation:
		_, _, authorization = authorizeNativeReattachment(os.Getenv)
	case nativeRestoreReclaimOperation, nativeRestoreReclaimResume:
		_, authorization = authorizedNativeRestoreReclaimRun(os.Getenv)
	case "inventory-replicas":
		authorization = authorizeInventory(os.Getenv, target)
	case "inspect-host-memory":
		authorization = authorizeHostMemory(os.Getenv, target)
	case "reboot-host-for-hugepages":
		authorization = authorizeHostReboot(os.Getenv, target)
	case "observe-database":
		authorization = authorizeCNPGObservation(os.Getenv)
	case observeCorootKeeperOperation:
		authorization = authorizeCorootKeeperObservation(os.Getenv)
	case observeCorootClickHouseOperation:
		authorization = authorizeCorootClickHouseObservation(os.Getenv)
	case corootKeeperCutoverOperation:
		_, authorization = authorizeCorootKeeperCutover(os.Getenv)
	case transferCorootKeeperOperation:
		_, authorization = authorizeCorootKeeperOperation(os.Getenv, operation)
	case openBaoPairOperation:
		_, _, authorization = authorizeOpenBaoPair(os.Getenv)
	case openBaoPairResumeOperation:
		_, _, authorization = authorizeOpenBaoPairResume(os.Getenv)
	case cnpgArchiveProofOperation:
		authorization = authorizeCNPGArchiveProof(os.Getenv)
	case umamiArchiveIsolationResumeOperation:
		_, authorization = authorizeUmamiArchiveIsolationResume(os.Getenv)
	case umamiArchiveDataResumeOperation:
		_, authorization = authorizeUmamiArchiveDataResume(os.Getenv)
	case umamiStandbyObservationOperation:
		_, _, authorization = authorizeUmamiStandbyObservation(os.Getenv)
	case providerCNPGStandbyObservation:
		_, authorization = authorizeProviderCNPGStandby(os.Getenv)
	case umamiStandbyReplacementOperation, umamiV2SwitchoverOperation:
		_, _, authorization = authorizeUmamiStandbyTarget(os.Getenv, operation)
	case weddingV2SwitchoverOperation:
		_, _, authorization = authorizeCNPGStandbyTarget(os.Getenv, operation, weddingStandbySource.Name)
	case resumeUmamiStandbyOperation:
		_, authorization = authorizeUmamiStandbyResume(os.Getenv)
	case recoverUmamiStandbyNameOperation:
		_, authorization = authorizeUmamiStandbyNameRecovery(os.Getenv)
	case replaceWeddingStandbyOperation, resumeWeddingStandbyOperation, recoverWeddingStandbyOperation:
		_, authorization = authorizeWeddingStandbyOperation(os.Getenv, operation)
	case "repair-v1-reservation":
		authorization = authorizeV1ReservationRepair(os.Getenv, target)
	case "reserve-v1-scratch":
		authorization = authorizeV1ScratchHeadroom(os.Getenv, target)
	case "reserve-v1-backup-scratch":
		authorization = authorizeV1BackupHeadroom(os.Getenv)
	case prepareActualBudgetScratch, retireActualBudgetScratch:
		_, authorization = authorizeActualBudgetScratch(os.Getenv)
	case actualBudgetBackupOperation:
		authorization = authorizeActualBudgetBackup(os.Getenv)
	case actualBudgetRestoreOperation, actualBudgetRestoreResume, actualBudgetRestoreRetry:
		_, authorization = authorizeActualBudgetRestore(os.Getenv)
	case detachedV1SourceOperation:
		detachedVolume, authorization = authorizeDetachedV1Source(os.Getenv)
	case retainV1SourceOperation:
		authorization = authorizeV1SourceRetention(os.Getenv)
	case retainProviderSourceOperation:
		authorization = authorizeProviderSourceRetention(os.Getenv)
	case "inventory-storage-root":
		authorization = authorizeStorageRoot(os.Getenv, target)
	case "inventory-storage-layout":
		authorization = authorizeStorageLayout(os.Getenv, target)
	case "inventory-provider-volume-footprint":
		authorization = authorizeProviderVolumeFootprint(os.Getenv)
	case runtimeInventoryOperation:
		authorization = authorizeRuntimeInventory(os.Getenv)
	default:
		authorization = authorizeRefresh(os.Getenv)
	}
	if authorization != nil {
		fmt.Fprintln(os.Stderr, authorization)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Minute)
	defer cancel()
	if operation == detachedV1SourceOperation {
		c := client{run: detachedV1SourceReadCommand, context: "admin@prod"}
		receipt, err := c.observeDetachedV1Source(ctx, os.Getenv("GITHUB_SHA"), detachedVolume)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Detached V1 source qualification held")
			os.Exit(1)
		}
		fmt.Printf("DETACHED_V1_SOURCE=VERIFIED phase=%s bytes=%d digest=%s writes=0\n", receipt.Phase, receipt.Bytes, receipt.Digest)
		return
	}
	if operation == prepareActualBudgetScratch || operation == retireActualBudgetScratch {
		confirmation, err := authorizeActualBudgetScratch(os.Getenv)
		if err == nil {
			err = runActualBudgetScratch(ctx, client{run: execute, context: "admin@prod"}, operation,
				os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), confirmation)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Actual Budget staging reservation held:", err)
			os.Exit(1)
		}
		fmt.Println("ACTUAL_BUDGET_BACKUP_SCRATCH=VERIFIED")
		return
	}
	if operation == actualBudgetBackupOperation {
		digest := os.Getenv("LONGHORN_V2_CONFIRM")[len(actualBudgetBackupOperation)+1:]
		if err := runActualBudgetBackup(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), digest); err != nil {
			fmt.Fprintln(os.Stderr, "Actual Budget archive held:", err)
			os.Exit(1)
		}
		fmt.Println("ACTUAL_BUDGET_V1_BACKUP=VERIFIED transfer=KOPIA sourceBinding=UNCHANGED restore=UNVERIFIED")
		return
	}
	if operation == nativeSeedOperation {
		if err := runNativeSeed(ctx, client{run: execute, context: "admin@prod", nativeSeed: true}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, "Disposable canary source held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and assets for explicit recovery; no V2 restore or migration is accepted.")
			os.Exit(1)
		}
		fmt.Println("LONGHORN_NATIVE_SEED=SOURCE_CLEANED transfer=KOPIA v2Restore=UNVERIFIED reattachment=UNVERIFIED migration=UNVERIFIED")
		return
	}
	if operation == runtimeInventoryOperation {
		receipt, err := observeRuntimeInventory(ctx, client{run: runtimeInventoryReadCommand, context: "admin@prod"}, os.Getenv("LONGHORN_RUNTIME_INVENTORY_SOURCE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("LONGHORN_RUNTIME_INVENTORY=OBSERVED kubernetesNodes=%d storageNodes=%d pods=%d instanceManagers=%d v1Volumes=%d v2Volumes=%d default=%s epochSHA256=%s declaredOverlap=%s overlapNodes=%d adoption=UNVERIFIED headroom=UNVERIFIED writes=0\n", receipt.KubernetesNodes, receipt.Nodes, receipt.Pods, receipt.InstanceManagers, receipt.V1Volumes, receipt.V2Volumes, receipt.Default, receipt.EpochSHA, receipt.DeclaredOverlap, receipt.OverlapNodes)
		return
	}
	if operation == "inventory-provider-volume-footprint" {
		receipt, err := observeProviderVolumeFootprint(ctx, client{run: providerFootprintReadCommand, context: "admin@prod"}, os.Getenv("GITHUB_SHA"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Provider volume footprint held:", err)
			os.Exit(1)
		}
		fmt.Printf("LONGHORN_PROVIDER_VOLUME_FOOTPRINT=OBSERVED claims=%d measured=%d unmeasured=%d requestedGiBUpper=%d measuredUsedGiBUpper=%d unmeasuredRequestedGiBUpper=%d\n",
			receipt.Claims, receipt.Measured, receipt.Unmeasured, providerUsageGiBUpper(receipt.RequestedBytes),
			providerUsageGiBUpper(receipt.MeasuredUsedBytes), providerUsageGiBUpper(receipt.UnmeasuredRequestedBytes))
		return
	}
	if operation == "observe-database" {
		observationContext, stop := context.WithTimeout(ctx, 5*time.Minute)
		defer stop()
		observation, err := observeCNPGMigration(observationContext, client{run: execute, context: "admin@prod"}, os.Getenv("LONGHORN_V2_DATABASE"), os.Getenv("GITHUB_SHA"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Database migration observation held:", err)
			os.Exit(1)
		}
		fmt.Printf("CNPG_MIGRATION_OBSERVATION=PASS databases=1 instances=%d writes=0\n", len(observation.States))
		return
	}
	if operation == "repair-v1-reservation" {
		if err := repairV1Reservation(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA")); err != nil {
			fmt.Fprintln(os.Stderr, "V1 reservation repair held:", err)
			os.Exit(1)
		}
		return
	}
	if operation == "reserve-v1-scratch" {
		if err := reserveV1ScratchHeadroom(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA")); err != nil {
			fmt.Fprintln(os.Stderr, "V1 scratch reservation held:", err)
			os.Exit(1)
		}
		return
	}
	if operation == "reserve-v1-backup-scratch" {
		if err := reserveV1BackupHeadroom(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA")); err != nil {
			fmt.Fprintln(os.Stderr, "V1 backup staging reservation held:", err)
			os.Exit(1)
		}
		return
	}
	if operation == retainV1SourceOperation || operation == retainProviderSourceOperation {
		retain := retainV1Source
		if operation == retainProviderSourceOperation {
			retain = retainProviderSource
		}
		if err := retain(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("LONGHORN_V2_CONFIRM")); err != nil {
			fmt.Fprintln(os.Stderr, "Source retention held:", err)
			os.Exit(1)
		}
		return
	}
	if operation == "inventory-storage-layout" {
		if err := runStorageLayoutInventory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "inspect-host-memory" {
		if err := runHostMemory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "inventory-storage-root" {
		if err := runStorageRootInventory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "inventory-replicas" {
		if err := runReplicaInventory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == recoverUnstartedV1Operation {
		if err := recoverUnstartedV1Benchmark(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, "Unstarted V1 benchmark recovery held:", err)
			os.Exit(1)
		}
		fmt.Println("LONGHORN_V1_UNSTARTED_RECOVERY=COMPLETE")
		return
	}
	if operation == recoverBoundV1Operation {
		original := strings.TrimPrefix(os.Getenv("LONGHORN_V2_CONFIRM"), recoverBoundV1Confirmation)
		if err := recoverBoundV1Benchmark(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), original); err != nil {
			fmt.Fprintln(os.Stderr, "Bound V1 benchmark recovery held:", err)
			os.Exit(1)
		}
		fmt.Println("LONGHORN_V1_BOUND_RECOVERY=COMPLETE")
		return
	}
	if operation == "benchmark-v1" || operation == "benchmark-v2" {
		if err := runBenchmark(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), operation[len("benchmark-"):]); err != nil {
			fmt.Fprintln(os.Stderr, "Storage benchmark held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and scratch resources for explicit recovery; no baseline is accepted.")
			os.Exit(1)
		}
		return
	}
	config, err := os.ReadFile("ksail.prod.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Reviewed capacity configuration is unreadable.")
		os.Exit(1)
	}
	if operation == nativeRestoreOperation || operation == nativeRestoreResumeOperation || operation == nativeReattachmentOperation {
		c := client{run: execute, context: "admin@prod"}
		switch operation {
		case nativeReattachmentOperation:
			err = resumeNativeReattachment(ctx, c, config)
		case nativeRestoreResumeOperation:
			err = resumeNativeRestore(ctx, c, config)
		default:
			err = runNativeRestore(ctx, c, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), config)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Disposable V2 restore held:", err)
			fmt.Fprintln(os.Stderr, "Preserve the seed, restore journal and its assets for explicit recovery.")
			os.Exit(1)
		}
		fmt.Println("LONGHORN_NATIVE_RESTORE=VERIFIED transfer=KOPIA engine=v2 reattachment=VERIFIED migration=UNVERIFIED")
		return
	}
	if operation == nativeRestoreReclaimOperation || operation == nativeRestoreReclaimResume {
		if err := runNativeRestoreReclaim(ctx, client{run: execute, context: "admin@prod"}, config); err != nil {
			fmt.Fprintln(os.Stderr, "Disposable V2 reclaim held:", err)
			fmt.Fprintln(os.Stderr, "Preserve the reclaim journal and any confirmed admission policies for explicit recovery.")
			os.Exit(1)
		}
		fmt.Println("LONGHORN_NATIVE_RESTORE_RECLAIM=COMPLETE engine=v2 sourceBackup=RETAINED")
		return
	}
	pools, err := configuredMigrationPools(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if operation == observeCorootKeeperOperation {
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		receipt, err := observeCorootKeeperQuorum(ctx, c, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Coroot Keeper observation held:", err)
			os.Exit(1)
		}
		fmt.Println("COROOT_KEEPER_QUORUM=VERIFIED members=3 syncedFollowers=2 disruptionBudget=VERIFIED writes=0")
		for i, member := range receipt.Members {
			if !member.Leader && receipt.Disks[i].Digest != "" {
				fmt.Printf("COROOT_KEEPER_FOLLOWER=QUALIFIED ordinal=%d digest=%s\n", i, receipt.Disks[i].Digest)
			}
		}
		return
	}
	if operation == observeCorootClickHouseOperation {
		c := client{run: execute, context: "admin@prod"}
		receipt, err := observeCorootClickHouse(ctx, c, os.Getenv("GITHUB_SHA"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Coroot ClickHouse observation held:", err)
			os.Exit(1)
		}
		fmt.Printf("COROOT_CLICKHOUSE_STORAGE=VERIFIED members=2 providerVolumes=2 tables=%d replicated=%d local=%d views=%d maxReplicaDelaySeconds=%d writes=0\n",
			receipt.Tables, receipt.Replicated, receipt.Local, receipt.Views, receipt.MaxReplicaDelaySeconds)
		return
	}
	if operation == corootKeeperCutoverOperation {
		input, err := authorizeCorootKeeperCutover(os.Getenv)
		if err == nil {
			c := client{run: execute, context: "admin@prod", inventoryPools: pools}
			err = runCorootKeeperCutover(ctx, c, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), input)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("COROOT_KEEPER_REPLACEMENT=VERIFIED originalStorage=RETAINED engine=v2 replicas=3 raft=SYNCED")
		return
	}
	if operation == transferCorootKeeperOperation {
		input, err := authorizeCorootKeeperOperation(os.Getenv, operation)
		if err == nil {
			c := client{run: execute, context: "admin@prod", inventoryPools: pools}
			err = runCorootKeeperLeadershipTransfer(ctx, c, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), input)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Coroot Keeper leadership held:", err)
			os.Exit(1)
		}
		fmt.Println("COROOT_KEEPER_V2_LEADER=VERIFIED quorum=HEALTHY providerMember=FOLLOWER")
		return
	}
	if operation == openBaoPairOperation {
		input, prior, err := authorizeOpenBaoPair(os.Getenv)
		if err == nil {
			c := client{run: execute, context: "admin@prod", inventoryPools: pools}
			err = runOpenBaoPairCutover(ctx, c, input, prior, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"),
				os.Getenv("HCLOUD_TOKEN"), os.Getenv("OPENBAO_VERSION"))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("OPENBAO_V2_PAIR=VERIFIED volumes=2 replicas=3 originalStorage=RETAINED raft=VERIFIED secrets=VERIFIED")
		return
	}
	if operation == openBaoPairResumeOperation {
		input, prior, err := authorizeOpenBaoPairResume(os.Getenv)
		var migrated bool
		if err == nil {
			c := client{run: execute, context: "admin@prod", inventoryPools: pools}
			migrated, err = runOpenBaoPairResume(ctx, c, input, prior, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"),
				os.Getenv("HCLOUD_TOKEN"), os.Getenv("OPENBAO_VERSION"))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if migrated {
			fmt.Println("OPENBAO_V2_PAIR=VERIFIED volumes=2 replicas=3 originalStorage=RETAINED raft=VERIFIED secrets=VERIFIED")
		} else {
			fmt.Println("OPENBAO_ORIGINAL_PAIR=RESTORED volumes=2 raft=VERIFIED")
		}
		return
	}
	if operation == temporaryReserveObservation {
		observing, stop := context.WithTimeout(ctx, 20*time.Minute)
		defer stop()
		activation := temporaryV2Activation{
			c:      client{run: temporaryReserveReadCommand, context: "admin@prod", inventoryPools: pools},
			source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools,
		}
		if err := activation.observeUnusedReserve(observing); err != nil {
			fmt.Fprintln(os.Stderr, "Retained V2 capacity observation held:", err)
			os.Exit(1)
		}
		fmt.Println("LONGHORN_V2_RETAINED_CAPACITY=VERIFIED usableExtentGiB=96 additionalFailureDomains=0 writes=0")
		return
	}
	if operation == actualBudgetRestoreOperation || operation == actualBudgetRestoreResume || operation == actualBudgetRestoreRetry {
		confirmation, err := authorizeActualBudgetRestore(os.Getenv)
		if err == nil {
			c := client{run: execute, context: "admin@prod", inventoryPools: pools}
			err = runActualBudgetRestore(ctx, c, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), confirmation)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Actual Budget isolated restore held:", err)
			os.Exit(1)
		}
		fmt.Println("ACTUAL_BUDGET_V2_RESTORE=VERIFIED transfer=KOPIA replicas=3 source=RETAINED reader=RETIRED")
		return
	}
	if operation == cnpgArchiveProofOperation || operation == umamiArchiveIsolationResumeOperation || operation == umamiArchiveDataResumeOperation {
		runProof := runCNPGArchiveProof
		switch operation {
		case umamiArchiveIsolationResumeOperation:
			runProof = resumeUmamiArchiveIsolation
		case umamiArchiveDataResumeOperation:
			runProof = resumeUmamiArchiveData
		}
		if err := runProof(ctx, client{run: execute, context: "admin@prod", inventoryPools: pools}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("CNPG_ARCHIVE_PROOF=RESTORED engine=v2 physicalHistory=VERIFIED networkIsolation=VERIFIED archiveCredential=RETIRED storageCleanup=REQUIRED migration=UNVERIFIED")
		return
	}
	if operation == umamiStandbyReplacementOperation || operation == resumeUmamiStandbyOperation || operation == recoverUmamiStandbyNameOperation {
		replacementContext, stop := context.WithTimeout(ctx, 40*time.Minute)
		defer stop()
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		if err := executeUmamiStandbyReplacement(replacementContext, c, operation); err != nil {
			fmt.Fprintln(os.Stderr, "Umami standby replacement held:", err)
			os.Exit(1)
		}
		fmt.Println("UMAMI_V2_STANDBY_REPLACEMENT=VERIFIED originalStorage=RETAINED replicas=3 physicalReplay=VERIFIED")
		return
	}
	if operation == replaceWeddingStandbyOperation || operation == resumeWeddingStandbyOperation || operation == recoverWeddingStandbyOperation {
		replacementContext, stop := context.WithTimeout(ctx, 30*time.Minute)
		defer stop()
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		if err := runWeddingStandbyOperation(replacementContext, c, os.Getenv, operation); err != nil {
			fmt.Fprintln(os.Stderr, "Wedding standby replacement held:", err)
			os.Exit(1)
		}
		fmt.Println("WEDDING_V2_STANDBY_REPLACEMENT=VERIFIED originalStorage=RETAINED replicas=3 physicalReplay=VERIFIED")
		return
	}
	if operation == umamiV2SwitchoverOperation || operation == weddingV2SwitchoverOperation {
		database := cnpgSource{Namespace: "umami", Name: "umami-db"}
		if operation == weddingV2SwitchoverOperation {
			database = weddingStandbySource
		}
		previous, target, err := authorizeCNPGStandbyTarget(os.Getenv, operation, database.Name)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switchoverContext, stop := context.WithTimeout(ctx, 30*time.Minute)
		defer stop()
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		if err := runCNPGV2Switchover(switchoverContext, c, database, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), previous, target); err != nil {
			fmt.Fprintln(os.Stderr, "Database V2 switchover held:", err)
			os.Exit(1)
		}
		if operation == weddingV2SwitchoverOperation {
			fmt.Println("WEDDING_V2_PRIMARY=VERIFIED originalPrimary=RETAINED physicalHistory=VERIFIED")
		} else {
			fmt.Println("UMAMI_V2_PRIMARY=VERIFIED originalPrimary=RETAINED physicalHistory=VERIFIED")
		}
		return
	}
	if operation == umamiStandbyObservationOperation {
		target, spare, err := authorizeUmamiStandbyObservation(os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		observationContext, stop := context.WithTimeout(ctx, 10*time.Minute)
		defer stop()
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		source := os.Getenv("GITHUB_SHA")
		witness, err := observeUmamiStandbyCutover(observationContext, c, source, target, spare)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Umami standby observation held:", err)
			os.Exit(1)
		}
		if err := verifyUmamiStandbyEvictionAdmission(observationContext, c, source, witness); err != nil {
			fmt.Fprintln(os.Stderr, "Umami standby admission held:", err)
			os.Exit(1)
		}
		fmt.Println("UMAMI_V2_STANDBY=VERIFIED replicas=3 originalStorage=RETAINED physicalReplay=VERIFIED evictionAdmission=VERIFIED writes=0")
		return
	}
	if operation == providerCNPGStandbyObservation {
		grant, err := authorizeProviderCNPGStandby(os.Getenv)
		if err == nil {
			observationContext, stop := context.WithTimeout(ctx, 10*time.Minute)
			defer stop()
			c := client{run: execute, context: "admin@prod", inventoryPools: pools}
			err = observeProviderCNPGStandby(observationContext, c, grant)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Provider database V2 standby held:", err)
			os.Exit(1)
		}
		fmt.Println("PROVIDER_CNPG_V2_STANDBY=VERIFIED replicas=3 sources=ATTACHED physicalReplay=VERIFIED writes=0")
		return
	}
	if operation == "register-temporary-v2-storage" {
		legacyReceipts, err := parseLegacyPreparationReceipts(os.Getenv("LONGHORN_V2_LEGACY_PREPARATION_RECEIPTS"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		activation := temporaryV2Activation{c: client{run: execute, context: "admin@prod", inventoryPools: pools}, source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools, legacyReceipts: legacyReceipts}
		if err = activation.run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Temporary V2 registration held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its identity journal and any partial registration for explicit recovery.")
			os.Exit(1)
		}
		return
	}
	if operation == "prepare-unused-temporary-worker" || operation == "prepare-retained-temporary-worker" || operation == observeTemporaryRecoveryOperation {
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		if operation == observeTemporaryRecoveryOperation {
			c.run = temporaryRecoveryReadCommand
		}
		nodes, readErr := c.list(ctx, "nodes", "")
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "Temporary preparation held: node inventory is unreadable.")
			os.Exit(1)
		}
		resolved, _, resolveErr := resolveTemporaryInventoryTarget(target, nodes, pools)
		if resolveErr != nil {
			// Recovery may begin with the selected worker's owned cordon. A
			// journal, not the ordinal alone, must prove that exception.
			cm, cmErr := c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-prepare-"+target, true)
			if cmErr != nil || cm == nil {
				fmt.Fprintln(os.Stderr, "Temporary preparation held: reviewed pool selection is unproven.")
				os.Exit(1)
			}
			record, recordErr := decodePreparationJournal([]byte(text(cm, "data", "record")))
			if recordErr != nil || record.TemporarySelection != target {
				fmt.Fprintln(os.Stderr, "Temporary preparation held: recovery identity is unproven.")
				os.Exit(1)
			}
			if record.Phase == "reset-requested" {
				resolved, resolveErr = resolveTemporaryResetRecovery(target, nodes, pools, record)
			} else {
				view, viewErr := temporaryInventoryNodes(nodes, &record)
				if viewErr != nil {
					fmt.Fprintln(os.Stderr, "Temporary preparation held: owned cordon is unproven.")
					os.Exit(1)
				}
				resolved, _, resolveErr = resolveTemporaryInventoryTarget(target, view, pools)
			}
			if resolveErr != nil || resolved != record.Target {
				fmt.Fprintln(os.Stderr, "Temporary preparation held: recovery roster changed.")
				os.Exit(1)
			}
		}
		p := temporaryPreparation{preparation: preparation{c: c, target: resolved, name: "longhorn-v2-prepare-" + target, temporarySelection: target, source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}, staging: true}
		if operation == observeTemporaryRecoveryOperation {
			if err = p.observeRecovery(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "Temporary worker recovery observation held:", err)
				os.Exit(1)
			}
			fmt.Println("TEMPORARY_WORKER_RECOVERY=OBSERVED writes=0")
			return
		}
		if err = p.prepare(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Temporary worker preparation held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and owned cordon; uncertain resets are never replayed.")
			os.Exit(1)
		}
		return
	}
	if operation == "reboot-host-for-hugepages" {
		h := hostMemoryReboot{c: client{run: execute, context: "admin@prod"}, target: target, source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}
		if err = h.run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Host memory maintenance held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and owned cordon; uncertain reboot requests are never repeated.")
			os.Exit(1)
		}
		if _, err = fmt.Fprintln(os.Stdout, "Host memory maintenance recovered with original storage identities, native allocation, advertised capacity and database identity proved."); err != nil {
			os.Exit(1)
		}
		return
	}
	if operation == "prepare-worker" {
		p := preparation{c: client{run: execute, context: "admin@prod"}, target: target, name: "longhorn-v2-prepare-" + target,
			source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}
		if err = p.prepare(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Node preparation held:", err)
			fmt.Fprintln(os.Stderr, "Preserve the journal and any owned cordon; diagnose before a new confirmed main dispatch.")
			os.Exit(1)
		}
		return
	}
	if err = refreshClients(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), pools); err != nil {
		fmt.Fprintln(os.Stderr, "Worker client refresh held:", err)
		os.Exit(1)
	}
}

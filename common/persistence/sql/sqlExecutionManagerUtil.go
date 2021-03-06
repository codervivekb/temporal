// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sql

import (
	"bytes"
	"database/sql"
	"fmt"
	"time"

	"github.com/gogo/protobuf/types"
	executiongenpb "github.com/temporalio/temporal/.gen/proto/execution"
	executionpb "go.temporal.io/temporal-proto/execution"
	"go.temporal.io/temporal-proto/serviceerror"

	commongenpb "github.com/temporalio/temporal/.gen/proto/common"
	"github.com/temporalio/temporal/.gen/proto/persistenceblobs"
	replicationgenpb "github.com/temporalio/temporal/.gen/proto/replication"
	"github.com/temporalio/temporal/common"
	p "github.com/temporalio/temporal/common/persistence"
	"github.com/temporalio/temporal/common/persistence/serialization"
	"github.com/temporalio/temporal/common/persistence/sql/sqlplugin"
	"github.com/temporalio/temporal/common/primitives"
)

func applyWorkflowMutationTx(
	tx sqlplugin.Tx,
	shardID int,
	workflowMutation *p.InternalWorkflowMutation,
) error {

	executionInfo := workflowMutation.ExecutionInfo
	replicationState := workflowMutation.ReplicationState
	versionHistories := workflowMutation.VersionHistories
	startVersion := workflowMutation.StartVersion
	lastWriteVersion := workflowMutation.LastWriteVersion
	namespaceID := primitives.MustParseUUID(executionInfo.NamespaceID)
	workflowID := executionInfo.WorkflowID
	runID := primitives.MustParseUUID(executionInfo.RunID)

	// TODO remove once 2DC is deprecated
	//  since current version is only used by 2DC
	currentVersion := lastWriteVersion
	if replicationState != nil {
		currentVersion = replicationState.CurrentVersion
	}

	// TODO Remove me if UPDATE holds the lock to the end of a transaction
	if err := lockAndCheckNextEventID(tx,
		shardID,
		namespaceID,
		workflowID,
		runID,
		workflowMutation.Condition); err != nil {
		switch err.(type) {
		case *p.ConditionFailedError:
			return err
		default:
			return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Failed to lock executions row. Error: %v", err))
		}
	}

	if err := updateExecution(tx,
		executionInfo,
		replicationState,
		versionHistories,
		startVersion,
		lastWriteVersion,
		currentVersion,
		shardID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Failed to update executions row. Erorr: %v", err))
	}

	if err := applyTasks(tx,
		shardID,
		namespaceID,
		workflowID,
		runID,
		workflowMutation.TransferTasks,
		workflowMutation.ReplicationTasks,
		workflowMutation.TimerTasks); err != nil {
		return err
	}

	if err := updateActivityInfos(tx,
		workflowMutation.UpsertActivityInfos,
		workflowMutation.DeleteActivityInfos,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}

	if err := updateTimerInfos(tx,
		workflowMutation.UpsertTimerInfos,
		workflowMutation.DeleteTimerInfos,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}

	if err := updateChildExecutionInfos(tx,
		workflowMutation.UpsertChildExecutionInfos,
		workflowMutation.DeleteChildExecutionInfo,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}

	if err := updateRequestCancelInfos(tx,
		workflowMutation.UpsertRequestCancelInfos,
		workflowMutation.DeleteRequestCancelInfo,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}

	if err := updateSignalInfos(tx,
		workflowMutation.UpsertSignalInfos,
		workflowMutation.DeleteSignalInfo,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}

	if err := updateSignalsRequested(tx,
		workflowMutation.UpsertSignalRequestedIDs,
		workflowMutation.DeleteSignalRequestedID,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}

	if workflowMutation.ClearBufferedEvents {
		if err := deleteBufferedEvents(tx,
			shardID,
			namespaceID,
			workflowID,
			runID); err != nil {
			return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
		}
	}

	if err := updateBufferedEvents(tx,
		workflowMutation.NewBufferedEvents,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowMutationTx failed. Error: %v", err))
	}
	return nil
}

func applyWorkflowSnapshotTxAsReset(
	tx sqlplugin.Tx,
	shardID int,
	workflowSnapshot *p.InternalWorkflowSnapshot,
) error {

	executionInfo := workflowSnapshot.ExecutionInfo
	replicationState := workflowSnapshot.ReplicationState
	versionHistories := workflowSnapshot.VersionHistories
	startVersion := workflowSnapshot.StartVersion
	lastWriteVersion := workflowSnapshot.LastWriteVersion
	namespaceID := primitives.MustParseUUID(executionInfo.NamespaceID)
	workflowID := executionInfo.WorkflowID
	runID := primitives.MustParseUUID(executionInfo.RunID)

	// TODO remove once 2DC is deprecated
	//  since current version is only used by 2DC
	currentVersion := lastWriteVersion
	if replicationState != nil {
		currentVersion = replicationState.CurrentVersion
	}

	// TODO Is there a way to modify the various map tables without fear of other people adding rows after we delete, without locking the executions row?
	if err := lockAndCheckNextEventID(tx,
		shardID,
		namespaceID,
		workflowID,
		runID,
		workflowSnapshot.Condition); err != nil {
		switch err.(type) {
		case *p.ConditionFailedError:
			return err
		default:
			return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to lock executions row. Error: %v", err))
		}
	}

	if err := updateExecution(tx,
		executionInfo,
		replicationState,
		versionHistories,
		startVersion,
		lastWriteVersion,
		currentVersion,
		shardID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to update executions row. Erorr: %v", err))
	}

	if err := applyTasks(tx,
		shardID,
		namespaceID,
		workflowID,
		runID,
		workflowSnapshot.TransferTasks,
		workflowSnapshot.ReplicationTasks,
		workflowSnapshot.TimerTasks); err != nil {
		return err
	}

	if err := deleteActivityInfoMap(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear activity info map. Error: %v", err))
	}

	if err := updateActivityInfos(tx,
		workflowSnapshot.ActivityInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to insert into activity info map after clearing. Error: %v", err))
	}

	if err := deleteTimerInfoMap(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear timer info map. Error: %v", err))
	}

	if err := updateTimerInfos(tx,
		workflowSnapshot.TimerInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to insert into timer info map after clearing. Error: %v", err))
	}

	if err := deleteChildExecutionInfoMap(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear child execution info map. Error: %v", err))
	}

	if err := updateChildExecutionInfos(tx,
		workflowSnapshot.ChildExecutionInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to insert into activity info map after clearing. Error: %v", err))
	}

	if err := deleteRequestCancelInfoMap(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear request cancel info map. Error: %v", err))
	}

	if err := updateRequestCancelInfos(tx,
		workflowSnapshot.RequestCancelInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to insert into request cancel info map after clearing. Error: %v", err))
	}

	if err := deleteSignalInfoMap(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear signal info map. Error: %v", err))
	}

	if err := updateSignalInfos(tx,
		workflowSnapshot.SignalInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to insert into signal info map after clearing. Error: %v", err))
	}

	if err := deleteSignalsRequestedSet(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear signals requested set. Error: %v", err))
	}

	if err := updateSignalsRequested(tx,
		workflowSnapshot.SignalRequestedIDs,
		"",
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to insert into signals requested set after clearing. Error: %v", err))
	}

	if err := deleteBufferedEvents(tx,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsReset failed. Failed to clear buffered events. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) applyWorkflowSnapshotTxAsNew(
	tx sqlplugin.Tx,
	shardID int,
	workflowSnapshot *p.InternalWorkflowSnapshot,
) error {

	executionInfo := workflowSnapshot.ExecutionInfo
	replicationState := workflowSnapshot.ReplicationState
	versionHistories := workflowSnapshot.VersionHistories
	startVersion := workflowSnapshot.StartVersion
	lastWriteVersion := workflowSnapshot.LastWriteVersion
	namespaceID := primitives.MustParseUUID(executionInfo.NamespaceID)
	workflowID := executionInfo.WorkflowID
	runID := primitives.MustParseUUID(executionInfo.RunID)

	// TODO remove once 2DC is deprecated
	//  since current version is only used by 2DC
	currentVersion := lastWriteVersion
	if replicationState != nil {
		currentVersion = replicationState.CurrentVersion
	}

	if err := m.createExecution(tx,
		executionInfo,
		replicationState,
		versionHistories,
		startVersion,
		lastWriteVersion,
		currentVersion,
		shardID); err != nil {
		return err
	}

	if err := applyTasks(tx,
		shardID,
		namespaceID,
		workflowID,
		runID,
		workflowSnapshot.TransferTasks,
		workflowSnapshot.ReplicationTasks,
		workflowSnapshot.TimerTasks); err != nil {
		return err
	}

	if err := updateActivityInfos(tx,
		workflowSnapshot.ActivityInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsNew failed. Failed to insert into activity info map after clearing. Error: %v", err))
	}

	if err := updateTimerInfos(tx,
		workflowSnapshot.TimerInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsNew failed. Failed to insert into timer info map after clearing. Error: %v", err))
	}

	if err := updateChildExecutionInfos(tx,
		workflowSnapshot.ChildExecutionInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsNew failed. Failed to insert into activity info map after clearing. Error: %v", err))
	}

	if err := updateRequestCancelInfos(tx,
		workflowSnapshot.RequestCancelInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsNew failed. Failed to insert into request cancel info map after clearing. Error: %v", err))
	}

	if err := updateSignalInfos(tx,
		workflowSnapshot.SignalInfos,
		nil,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsNew failed. Failed to insert into signal info map after clearing. Error: %v", err))
	}

	if err := updateSignalsRequested(tx,
		workflowSnapshot.SignalRequestedIDs,
		"",
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyWorkflowSnapshotTxAsNew failed. Failed to insert into signals requested set after clearing. Error: %v", err))
	}

	return nil
}

func applyTasks(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
	transferTasks []p.Task,
	replicationTasks []p.Task,
	timerTasks []p.Task,
) error {

	if err := createTransferTasks(tx,
		transferTasks,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyTasks failed. Failed to create transfer tasks. Error: %v", err))
	}

	if err := createReplicationTasks(tx,
		replicationTasks,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyTasks failed. Failed to create replication tasks. Error: %v", err))
	}

	if err := createTimerTasks(tx,
		timerTasks,
		shardID,
		namespaceID,
		workflowID,
		runID); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("applyTasks failed. Failed to create timer tasks. Error: %v", err))
	}

	return nil
}

// lockCurrentExecutionIfExists returns current execution or nil if none is found for the workflowID
// locking it in the DB
func lockCurrentExecutionIfExists(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
) (*sqlplugin.CurrentExecutionsRow, error) {

	rows, err := tx.LockCurrentExecutionsJoinExecutions(&sqlplugin.CurrentExecutionsFilter{
		ShardID: int64(shardID), NamespaceID: namespaceID, WorkflowID: workflowID})
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, serviceerror.NewInternal(fmt.Sprintf("lockCurrentExecutionIfExists failed. Failed to get current_executions row for (shard,namespace,workflow) = (%v, %v, %v). Error: %v", shardID, namespaceID, workflowID, err))
		}
	}
	size := len(rows)
	if size > 1 {
		return nil, serviceerror.NewInternal(fmt.Sprintf("lockCurrentExecutionIfExists failed. Multiple current_executions rows for (shard,namespace,workflow) = (%v, %v, %v).", shardID, namespaceID, workflowID))
	}
	if size == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func createOrUpdateCurrentExecution(
	tx sqlplugin.Tx,
	createMode p.CreateWorkflowMode,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
	state executiongenpb.WorkflowExecutionState,
	status executionpb.WorkflowExecutionStatus,
	createRequestID string,
	startVersion int64,
	lastWriteVersion int64,
) error {

	row := sqlplugin.CurrentExecutionsRow{
		ShardID:          int64(shardID),
		NamespaceID:      namespaceID,
		WorkflowID:       workflowID,
		RunID:            runID,
		CreateRequestID:  createRequestID,
		State:            state,
		Status:           status,
		StartVersion:     startVersion,
		LastWriteVersion: lastWriteVersion,
	}

	switch createMode {
	case p.CreateWorkflowModeContinueAsNew:
		if err := updateCurrentExecution(tx,
			shardID,
			namespaceID,
			workflowID,
			runID,
			createRequestID,
			state,
			status,
			row.StartVersion,
			row.LastWriteVersion); err != nil {
			return serviceerror.NewInternal(fmt.Sprintf("createOrUpdateCurrentExecution failed. Failed to continue as new. Error: %v", err))
		}
	case p.CreateWorkflowModeWorkflowIDReuse:
		if err := updateCurrentExecution(tx,
			shardID,
			namespaceID,
			workflowID,
			runID,
			createRequestID,
			state,
			status,
			row.StartVersion,
			row.LastWriteVersion); err != nil {
			return serviceerror.NewInternal(fmt.Sprintf("createOrUpdateCurrentExecution failed. Failed to reuse workflow ID. Error: %v", err))
		}
	case p.CreateWorkflowModeBrandNew:
		if _, err := tx.InsertIntoCurrentExecutions(&row); err != nil {
			return serviceerror.NewInternal(fmt.Sprintf("createOrUpdateCurrentExecution failed. Failed to insert into current_executions table. Error: %v", err))
		}
	case p.CreateWorkflowModeZombie:
		// noop
	default:
		return fmt.Errorf("createOrUpdateCurrentExecution failed. Unknown workflow creation mode: %v", createMode)
	}

	return nil
}

func lockAndCheckNextEventID(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
	condition int64,
) error {

	nextEventID, err := lockNextEventID(tx, shardID, namespaceID, workflowID, runID)
	if err != nil {
		return err
	}
	if *nextEventID != condition {
		return &p.ConditionFailedError{
			Msg: fmt.Sprintf("lockAndCheckNextEventID failed. Next_event_id was %v when it should have been %v.", nextEventID, condition),
		}
	}
	return nil
}

func lockNextEventID(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
) (*int64, error) {

	nextEventID, err := tx.WriteLockExecutions(&sqlplugin.ExecutionsFilter{
		ShardID:     shardID,
		NamespaceID: namespaceID,
		WorkflowID:  workflowID,
		RunID:       runID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(fmt.Sprintf("lockNextEventID failed. Unable to lock executions row with (shard, namespace, workflow, run) = (%v,%v,%v,%v) which does not exist.",
				shardID,
				namespaceID,
				workflowID,
				runID))
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("lockNextEventID failed. Error: %v", err))
	}
	result := int64(nextEventID)
	return &result, nil
}

func createTransferTasks(
	tx sqlplugin.Tx,
	transferTasks []p.Task,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
) error {

	if len(transferTasks) == 0 {
		return nil
	}

	transferTasksRows := make([]sqlplugin.TransferTasksRow, len(transferTasks))
	for i, task := range transferTasks {
		info := &persistenceblobs.TransferTaskInfo{
			NamespaceId:       namespaceID,
			WorkflowId:        workflowID,
			RunId:             runID,
			TargetNamespaceId: namespaceID,
			TargetWorkflowId:  p.TransferTaskTransferTargetWorkflowID,
			ScheduleId:        0,
			TaskId:            task.GetTaskID(),
		}

		transferTasksRows[i].ShardID = shardID
		transferTasksRows[i].TaskID = task.GetTaskID()

		switch task.GetType() {
		case commongenpb.TaskType_TransferActivityTask:
			info.TargetNamespaceId = primitives.MustParseUUID(task.(*p.ActivityTask).NamespaceID)
			info.TaskList = task.(*p.ActivityTask).TaskList
			info.ScheduleId = task.(*p.ActivityTask).ScheduleID

		case commongenpb.TaskType_TransferDecisionTask:
			info.TargetNamespaceId = primitives.MustParseUUID(task.(*p.DecisionTask).NamespaceID)
			info.TaskList = task.(*p.DecisionTask).TaskList
			info.ScheduleId = task.(*p.DecisionTask).ScheduleID

		case commongenpb.TaskType_TransferCancelExecution:
			info.TargetNamespaceId = primitives.MustParseUUID(task.(*p.CancelExecutionTask).TargetNamespaceID)
			info.TargetWorkflowId = task.(*p.CancelExecutionTask).TargetWorkflowID
			if task.(*p.CancelExecutionTask).TargetRunID != "" {
				info.TargetRunId = primitives.MustParseUUID(task.(*p.CancelExecutionTask).TargetRunID)
			}
			info.TargetChildWorkflowOnly = task.(*p.CancelExecutionTask).TargetChildWorkflowOnly
			info.ScheduleId = task.(*p.CancelExecutionTask).InitiatedID

		case commongenpb.TaskType_TransferSignalExecution:
			info.TargetNamespaceId = primitives.MustParseUUID(task.(*p.SignalExecutionTask).TargetNamespaceID)
			info.TargetWorkflowId = task.(*p.SignalExecutionTask).TargetWorkflowID
			if task.(*p.SignalExecutionTask).TargetRunID != "" {
				info.TargetRunId = primitives.MustParseUUID(task.(*p.SignalExecutionTask).TargetRunID)
			}
			info.TargetChildWorkflowOnly = task.(*p.SignalExecutionTask).TargetChildWorkflowOnly
			info.ScheduleId = task.(*p.SignalExecutionTask).InitiatedID

		case commongenpb.TaskType_TransferStartChildExecution:
			info.TargetNamespaceId = primitives.MustParseUUID(task.(*p.StartChildExecutionTask).TargetNamespaceID)
			info.TargetWorkflowId = task.(*p.StartChildExecutionTask).TargetWorkflowID
			info.ScheduleId = task.(*p.StartChildExecutionTask).InitiatedID

		case commongenpb.TaskType_TransferCloseExecution,
			commongenpb.TaskType_TransferRecordWorkflowStarted,
			commongenpb.TaskType_TransferResetWorkflow,
			commongenpb.TaskType_TransferUpsertWorkflowSearchAttributes:
			// No explicit property needs to be set

		default:
			return serviceerror.NewInternal(fmt.Sprintf("createTransferTasks failed. Unknow transfer type: %v", task.GetType()))
		}

		info.TaskType = task.GetType()
		info.Version = task.GetVersion()

		t, err := types.TimestampProto(task.GetVisibilityTimestamp().UTC())
		if err != nil {
			return err
		}

		info.VisibilityTimestamp = t

		blob, err := serialization.TransferTaskInfoToBlob(info)
		if err != nil {
			return err
		}
		transferTasksRows[i].Data = blob.Data
		transferTasksRows[i].DataEncoding = string(blob.Encoding)
	}

	result, err := tx.InsertIntoTransferTasks(transferTasksRows)
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("createTransferTasks failed. Error: %v", err))
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("createTransferTasks failed. Could not verify number of rows inserted. Error: %v", err))
	}

	if int(rowsAffected) != len(transferTasks) {
		return serviceerror.NewInternal(fmt.Sprintf("createTransferTasks failed. Inserted %v instead of %v rows into transfer_tasks. Error: %v", rowsAffected, len(transferTasks), err))
	}

	return nil
}

func createReplicationTasks(
	tx sqlplugin.Tx,
	replicationTasks []p.Task,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
) error {

	if len(replicationTasks) == 0 {
		return nil
	}
	replicationTasksRows := make([]sqlplugin.ReplicationTasksRow, len(replicationTasks))

	for i, task := range replicationTasks {

		firstEventID := common.EmptyEventID
		nextEventID := common.EmptyEventID
		version := common.EmptyVersion
		activityScheduleID := common.EmptyEventID
		var lastReplicationInfo map[string]*replicationgenpb.ReplicationInfo

		var branchToken, newRunBranchToken []byte
		var resetWorkflow bool

		switch task.GetType() {
		case commongenpb.TaskType_ReplicationHistory:
			historyReplicationTask, ok := task.(*p.HistoryReplicationTask)
			if !ok {
				return serviceerror.NewInternal(fmt.Sprintf("createReplicationTasks failed. Failed to cast %v to HistoryReplicationTask", task))
			}
			firstEventID = historyReplicationTask.FirstEventID
			nextEventID = historyReplicationTask.NextEventID
			version = task.GetVersion()
			branchToken = historyReplicationTask.BranchToken
			newRunBranchToken = historyReplicationTask.NewRunBranchToken
			resetWorkflow = historyReplicationTask.ResetWorkflow
			lastReplicationInfo = make(map[string]*replicationgenpb.ReplicationInfo, len(historyReplicationTask.LastReplicationInfo))
			for k, v := range historyReplicationTask.LastReplicationInfo {
				lastReplicationInfo[k] = &replicationgenpb.ReplicationInfo{Version: v.Version, LastEventId: v.LastEventId}
			}

		case commongenpb.TaskType_ReplicationSyncActivity:
			version = task.GetVersion()
			activityScheduleID = task.(*p.SyncActivityTask).ScheduledID
			lastReplicationInfo = map[string]*replicationgenpb.ReplicationInfo{}

		default:
			return serviceerror.NewInternal(fmt.Sprintf("Unknown replication task: %v", task.GetType()))
		}

		blob, err := serialization.ReplicationTaskInfoToBlob(&persistenceblobs.ReplicationTaskInfo{
			TaskId:                  task.GetTaskID(),
			NamespaceId:             namespaceID,
			WorkflowId:              workflowID,
			RunId:                   runID,
			TaskType:                task.GetType(),
			FirstEventId:            firstEventID,
			NextEventId:             nextEventID,
			Version:                 version,
			LastReplicationInfo:     lastReplicationInfo,
			ScheduledId:             activityScheduleID,
			EventStoreVersion:       p.EventStoreVersion,
			NewRunEventStoreVersion: p.EventStoreVersion,
			BranchToken:             branchToken,
			NewRunBranchToken:       newRunBranchToken,
			ResetWorkflow:           resetWorkflow,
		})
		if err != nil {
			return err
		}
		replicationTasksRows[i].ShardID = shardID
		replicationTasksRows[i].TaskID = task.GetTaskID()
		replicationTasksRows[i].Data = blob.Data
		replicationTasksRows[i].DataEncoding = string(blob.Encoding)
	}

	result, err := tx.InsertIntoReplicationTasks(replicationTasksRows)
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("createReplicationTasks failed. Error: %v", err))
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("createReplicationTasks failed. Could not verify number of rows inserted. Error: %v", err))
	}

	if int(rowsAffected) != len(replicationTasks) {
		return serviceerror.NewInternal(fmt.Sprintf("createReplicationTasks failed. Inserted %v instead of %v rows into transfer_tasks. Error: %v", rowsAffected, len(replicationTasks), err))
	}

	return nil
}

func createTimerTasks(
	tx sqlplugin.Tx,
	timerTasks []p.Task,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
) error {

	if len(timerTasks) > 0 {
		timerTasksRows := make([]sqlplugin.TimerTasksRow, len(timerTasks))

		for i, task := range timerTasks {
			info := &persistenceblobs.TimerTaskInfo{}
			switch t := task.(type) {
			case *p.DecisionTimeoutTask:
				info.EventId = t.EventID
				info.TimeoutType = int32(t.TimeoutType)
				info.ScheduleAttempt = t.ScheduleAttempt

			case *p.ActivityTimeoutTask:
				info.EventId = t.EventID
				info.TimeoutType = int32(t.TimeoutType)
				info.ScheduleAttempt = t.Attempt

			case *p.UserTimerTask:
				info.EventId = t.EventID

			case *p.ActivityRetryTimerTask:
				info.EventId = t.EventID
				info.ScheduleAttempt = int64(t.Attempt)

			case *p.WorkflowBackoffTimerTask:
				info.EventId = t.EventID
				info.TimeoutType = int32(t.TimeoutType)

			case *p.WorkflowTimeoutTask:
				// noop

			case *p.DeleteHistoryEventTask:
				// noop

			default:
				return serviceerror.NewInternal(fmt.Sprintf("createTimerTasks failed. Unknown timer task: %v", task.GetType()))
			}

			info.NamespaceId = namespaceID
			info.WorkflowId = workflowID
			info.RunId = runID
			info.Version = task.GetVersion()
			info.TaskType = task.GetType()
			info.TaskId = task.GetTaskID()

			goVisTs := task.GetVisibilityTimestamp()
			protoVisTs, err := types.TimestampProto(goVisTs)
			if err != nil {
				return err
			}

			info.VisibilityTimestamp = protoVisTs
			blob, err := serialization.TimerTaskInfoToBlob(info)
			if err != nil {
				return err
			}

			timerTasksRows[i].ShardID = shardID
			timerTasksRows[i].VisibilityTimestamp = goVisTs
			timerTasksRows[i].TaskID = task.GetTaskID()
			timerTasksRows[i].Data = blob.Data
			timerTasksRows[i].DataEncoding = string(blob.Encoding)
		}

		result, err := tx.InsertIntoTimerTasks(timerTasksRows)
		if err != nil {
			return serviceerror.NewInternal(fmt.Sprintf("createTimerTasks failed. Error: %v", err))
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return serviceerror.NewInternal(fmt.Sprintf("createTimerTasks failed. Could not verify number of rows inserted. Error: %v", err))
		}

		if int(rowsAffected) != len(timerTasks) {
			return serviceerror.NewInternal(fmt.Sprintf("createTimerTasks failed. Inserted %v instead of %v rows into timer_tasks. Error: %v", rowsAffected, len(timerTasks), err))
		}
	}

	return nil
}

func assertNotCurrentExecution(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
) error {
	currentRow, err := tx.LockCurrentExecutions(&sqlplugin.CurrentExecutionsFilter{
		ShardID:     int64(shardID),
		NamespaceID: namespaceID,
		WorkflowID:  workflowID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			// allow bypassing no current record
			return nil
		}
		return serviceerror.NewInternal(fmt.Sprintf("assertCurrentExecution failed. Unable to load current record. Error: %v", err))
	}
	return assertRunIDMismatch(runID, currentRow.RunID)
}

func assertRunIDAndUpdateCurrentExecution(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	newRunID primitives.UUID,
	previousRunID primitives.UUID,
	createRequestID string,
	state executiongenpb.WorkflowExecutionState,
	status executionpb.WorkflowExecutionStatus,
	startVersion int64,
	lastWriteVersion int64,
) error {

	assertFn := func(currentRow *sqlplugin.CurrentExecutionsRow) error {
		if !bytes.Equal(currentRow.RunID, previousRunID) {
			return &p.ConditionFailedError{Msg: fmt.Sprintf(
				"assertRunIDAndUpdateCurrentExecution failed. Current RunId was %v, expected %v",
				currentRow.RunID,
				previousRunID,
			)}
		}
		return nil
	}
	if err := assertCurrentExecution(tx, shardID, namespaceID, workflowID, assertFn); err != nil {
		return err
	}

	return updateCurrentExecution(tx, shardID, namespaceID, workflowID, newRunID, createRequestID, state, status, startVersion, lastWriteVersion)
}

func assertAndUpdateCurrentExecution(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	newRunID primitives.UUID,
	previousRunID primitives.UUID,
	previousLastWriteVersion int64,
	previousState executiongenpb.WorkflowExecutionState,
	createRequestID string,
	state executiongenpb.WorkflowExecutionState,
	status executionpb.WorkflowExecutionStatus,
	startVersion int64,
	lastWriteVersion int64,
) error {

	assertFn := func(currentRow *sqlplugin.CurrentExecutionsRow) error {
		if !bytes.Equal(currentRow.RunID, previousRunID) {
			return &p.ConditionFailedError{Msg: fmt.Sprintf(
				"assertAndUpdateCurrentExecution failed. Current run ID was %v, expected %v",
				currentRow.RunID,
				previousRunID,
			)}
		}
		if currentRow.LastWriteVersion != previousLastWriteVersion {
			return &p.ConditionFailedError{Msg: fmt.Sprintf(
				"assertAndUpdateCurrentExecution failed. Current last write version was %v, expected %v",
				currentRow.LastWriteVersion,
				previousLastWriteVersion,
			)}
		}
		if currentRow.State != previousState {
			return &p.ConditionFailedError{Msg: fmt.Sprintf(
				"assertAndUpdateCurrentExecution failed. Current state %v, expected %v",
				currentRow.State,
				previousState,
			)}
		}
		return nil
	}
	if err := assertCurrentExecution(tx, shardID, namespaceID, workflowID, assertFn); err != nil {
		return err
	}

	return updateCurrentExecution(tx, shardID, namespaceID, workflowID, newRunID, createRequestID, state, status, startVersion, lastWriteVersion)
}

func assertCurrentExecution(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	assertFn func(currentRow *sqlplugin.CurrentExecutionsRow) error,
) error {

	currentRow, err := tx.LockCurrentExecutions(&sqlplugin.CurrentExecutionsFilter{
		ShardID:     int64(shardID),
		NamespaceID: namespaceID,
		WorkflowID:  workflowID,
	})
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("assertCurrentExecution failed. Unable to load current record. Error: %v", err))
	}
	return assertFn(currentRow)
}

func assertRunIDMismatch(runID primitives.UUID, currentRunID primitives.UUID) error {
	// zombie workflow creation with existence of current record, this is a noop
	if bytes.Equal(currentRunID, runID) {
		return &p.ConditionFailedError{Msg: fmt.Sprintf(
			"assertRunIDMismatch failed. Current RunId was %v, input %v",
			currentRunID,
			runID,
		)}
	}
	return nil
}

func updateCurrentExecution(
	tx sqlplugin.Tx,
	shardID int,
	namespaceID primitives.UUID,
	workflowID string,
	runID primitives.UUID,
	createRequestID string,
	state executiongenpb.WorkflowExecutionState,
	status executionpb.WorkflowExecutionStatus,
	startVersion int64,
	lastWriteVersion int64,
) error {

	result, err := tx.UpdateCurrentExecutions(&sqlplugin.CurrentExecutionsRow{
		ShardID:          int64(shardID),
		NamespaceID:      namespaceID,
		WorkflowID:       workflowID,
		RunID:            runID,
		CreateRequestID:  createRequestID,
		State:            state,
		Status:           status,
		StartVersion:     startVersion,
		LastWriteVersion: lastWriteVersion,
	})
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("updateCurrentExecution failed. Error: %v", err))
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("updateCurrentExecution failed. Failed to check number of rows updated in current_executions table. Error: %v", err))
	}
	if rowsAffected != 1 {
		return serviceerror.NewInternal(fmt.Sprintf("updateCurrentExecution failed. %v rows of current_executions updated instead of 1.", rowsAffected))
	}
	return nil
}

func buildExecutionRow(
	executionInfo *p.InternalWorkflowExecutionInfo,
	replicationState *p.ReplicationState,
	versionHistories *serialization.DataBlob,
	startVersion int64,
	lastWriteVersion int64,
	currentVersion int64,
	shardID int,
) (row *sqlplugin.ExecutionsRow, err error) {

	info, state, err := p.InternalWorkflowExecutionInfoToProto(executionInfo, startVersion, currentVersion, replicationState, versionHistories)
	if err != nil {
		return nil, err
	}

	infoBlob, err := serialization.WorkflowExecutionInfoToBlob(info)
	if err != nil {
		return nil, err
	}

	stateBlob, err := serialization.WorkflowExecutionStateToBlob(state)
	if err != nil {
		return nil, err
	}

	return &sqlplugin.ExecutionsRow{
		ShardID:          shardID,
		NamespaceID:      primitives.MustParseUUID(executionInfo.NamespaceID),
		WorkflowID:       executionInfo.WorkflowID,
		RunID:            primitives.MustParseUUID(executionInfo.RunID),
		NextEventID:      executionInfo.NextEventID,
		LastWriteVersion: lastWriteVersion,
		Data:             infoBlob.Data,
		DataEncoding:     infoBlob.Encoding.String(),
		State:            stateBlob.Data,
		StateEncoding:    stateBlob.Encoding.String(),
	}, nil
}

func (m *sqlExecutionManager) createExecution(
	tx sqlplugin.Tx,
	executionInfo *p.InternalWorkflowExecutionInfo,
	replicationState *p.ReplicationState,
	versionHistories *serialization.DataBlob,
	startVersion int64,
	lastWriteVersion int64,
	currentVersion int64,
	shardID int,
) error {

	// validate workflow state & close status
	if err := p.ValidateCreateWorkflowStateStatus(
		executionInfo.State,
		executionInfo.Status); err != nil {
		return err
	}

	// TODO we should set the start time and last update time on business logic layer
	executionInfo.StartTimestamp = time.Now()
	executionInfo.LastUpdatedTimestamp = executionInfo.StartTimestamp

	row, err := buildExecutionRow(
		executionInfo,
		replicationState,
		versionHistories,
		startVersion,
		lastWriteVersion,
		currentVersion,
		shardID,
	)
	if err != nil {
		return err
	}
	result, err := tx.InsertIntoExecutions(row)
	if err != nil {
		if m.db.IsDupEntryError(err) {
			return &p.WorkflowExecutionAlreadyStartedError{
				Msg:              fmt.Sprintf("Workflow execution already running. WorkflowId: %v", executionInfo.WorkflowID),
				StartRequestID:   executionInfo.CreateRequestID,
				RunID:            executionInfo.RunID,
				State:            executionInfo.State,
				Status:           executionInfo.Status,
				LastWriteVersion: row.LastWriteVersion,
			}
		}
		return serviceerror.NewInternal(fmt.Sprintf("createExecution failed. Erorr: %v", err))
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("createExecution failed. Failed to verify number of rows affected. Erorr: %v", err))
	}
	if rowsAffected != 1 {
		return serviceerror.NewNotFound(fmt.Sprintf("createExecution failed. Affected %v rows updated instead of 1.", rowsAffected))
	}

	return nil
}

func updateExecution(
	tx sqlplugin.Tx,
	executionInfo *p.InternalWorkflowExecutionInfo,
	replicationState *p.ReplicationState,
	versionHistories *serialization.DataBlob,
	startVersion int64,
	lastWriteVersion int64,
	currentVersion int64,
	shardID int,
) error {

	// validate workflow state & close status
	if err := p.ValidateUpdateWorkflowStateStatus(
		executionInfo.State,
		executionInfo.Status); err != nil {
		return err
	}

	// TODO we should set the last update time on business logic layer
	executionInfo.LastUpdatedTimestamp = time.Now()

	row, err := buildExecutionRow(
		executionInfo,
		replicationState,
		versionHistories,
		startVersion,
		lastWriteVersion,
		currentVersion,
		shardID,
	)
	if err != nil {
		return err
	}
	result, err := tx.UpdateExecutions(row)
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("updateExecution failed. Erorr: %v", err))
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("updateExecution failed. Failed to verify number of rows affected. Erorr: %v", err))
	}
	if rowsAffected != 1 {
		return serviceerror.NewNotFound(fmt.Sprintf("updateExecution failed. Affected %v rows updated instead of 1.", rowsAffected))
	}

	return nil
}

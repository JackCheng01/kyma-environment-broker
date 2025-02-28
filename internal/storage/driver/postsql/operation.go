package postsql

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kyma-project/kyma-environment-broker/common/storage"
	"github.com/kyma-project/kyma-environment-broker/internal"
	"github.com/kyma-project/kyma-environment-broker/internal/storage/dberr"
	"github.com/kyma-project/kyma-environment-broker/internal/storage/dbmodel"
	"github.com/kyma-project/kyma-environment-broker/internal/storage/postsql"

	"github.com/pivotal-cf/brokerapi/v8/domain"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	Retrying   = "retrying" // to signal a retry sign before marking it to pending
	Succeeded  = "succeeded"
	Failed     = "failed"
	InProgress = "in progress"
)

type operations struct {
	postsql.Factory
	cipher Cipher
}

func NewOperation(sess postsql.Factory, cipher Cipher) *operations {
	return &operations{
		Factory: sess,
		cipher:  cipher,
	}
}

// InsertProvisioningOperation insert new ProvisioningOperation to storage
func (s *operations) InsertProvisioningOperation(operation internal.ProvisioningOperation) error {
	dto, err := s.provisioningOperationToDTO(&operation)
	if err != nil {
		return fmt.Errorf("while inserting provisioning operation (id: %s): %w", operation.ID, err)
	}

	return s.insert(dto)
}

// InsertOperation insert new Operation to storage
func (s *operations) InsertOperation(operation internal.Operation) error {
	dto, err := s.operationToDTO(&operation)

	if err != nil {
		return fmt.Errorf("while inserting operation (id: %s): %w", operation.ID, err)
	}

	return s.insert(dto)
}

// GetOperationByInstanceID fetches the latest Operation by given instanceID, returns error if not found
func (s *operations) GetOperationByInstanceID(instanceID string) (*internal.Operation, error) {

	op, err := s.getByInstanceID(instanceID)
	if err != nil {
		return nil, err
	}

	var operation internal.Operation
	err = json.Unmarshal([]byte(op.Data), &operation)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall provisioning data: %w", err)
	}

	ret, err := s.toOperation(op, operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return &ret, nil
}

// GetProvisioningOperationByID fetches the ProvisioningOperation by given ID, returns error if not found
func (s *operations) GetProvisioningOperationByID(operationID string) (*internal.ProvisioningOperation, error) {
	operation, err := s.getByID(operationID)
	if err != nil {
		return nil, fmt.Errorf("while getting operation by ID: %w", err)
	}

	ret, err := s.toProvisioningOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// GetProvisioningOperationByInstanceID fetches the latest ProvisioningOperation by given instanceID, returns error if not found
func (s *operations) GetProvisioningOperationByInstanceID(instanceID string) (*internal.ProvisioningOperation, error) {

	operation, err := s.getByTypeAndInstanceID(instanceID, internal.OperationTypeProvision)
	if err != nil {
		return nil, err
	}
	ret, err := s.toProvisioningOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// UpdateOperation updates Operation, fails if not exists or optimistic locking failure occurs.
func (s *operations) UpdateOperation(op internal.Operation) (*internal.Operation, error) {
	op.UpdatedAt = time.Now()
	dto, err := s.operationToDTO(&op)

	if err != nil {
		return nil, fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	lastErr := s.update(dto)
	op.Version = op.Version + 1

	return &op, lastErr
}

// UpdateProvisioningOperation updates ProvisioningOperation, fails if not exists or optimistic locking failure occurs.
func (s *operations) UpdateProvisioningOperation(op internal.ProvisioningOperation) (*internal.ProvisioningOperation, error) {
	op.UpdatedAt = time.Now()
	dto, err := s.provisioningOperationToDTO(&op)

	if err != nil {
		return nil, fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	lastErr := s.update(dto)
	op.Version = op.Version + 1

	return &op, lastErr
}

func (s *operations) ListProvisioningOperationsByInstanceID(instanceID string) ([]internal.ProvisioningOperation, error) {

	operations, err := s.listOperationsByInstanceIdAndType(instanceID, internal.OperationTypeProvision)
	if err != nil {
		return nil, fmt.Errorf("while loading operations list: %w", err)
	}

	ret, err := s.toProvisioningOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

func (s *operations) ListOperationsByInstanceID(instanceID string) ([]internal.Operation, error) {

	operations, err := s.listOperationsByInstanceId(instanceID)
	if err != nil {
		return nil, fmt.Errorf("while loading operations list: %w", err)
	}

	ret, err := s.toOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// InsertDeprovisioningOperation insert new DeprovisioningOperation to storage
func (s *operations) InsertDeprovisioningOperation(operation internal.DeprovisioningOperation) error {

	dto, err := s.deprovisioningOperationToDTO(&operation)
	if err != nil {
		return fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	return s.insert(dto)
}

// GetDeprovisioningOperationByID fetches the DeprovisioningOperation by given ID, returns error if not found
func (s *operations) GetDeprovisioningOperationByID(operationID string) (*internal.DeprovisioningOperation, error) {
	operation, err := s.getByID(operationID)
	if err != nil {
		return nil, fmt.Errorf("while getting operation by ID: %w", err)
	}

	ret, err := s.toDeprovisioningOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// GetDeprovisioningOperationByInstanceID fetches the latest DeprovisioningOperation by given instanceID, returns error if not found
func (s *operations) GetDeprovisioningOperationByInstanceID(instanceID string) (*internal.DeprovisioningOperation, error) {
	operation, err := s.getByTypeAndInstanceID(instanceID, internal.OperationTypeDeprovision)
	if err != nil {
		return nil, err
	}
	ret, err := s.toDeprovisioningOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// UpdateDeprovisioningOperation updates DeprovisioningOperation, fails if not exists or optimistic locking failure occurs.
func (s *operations) UpdateDeprovisioningOperation(operation internal.DeprovisioningOperation) (*internal.DeprovisioningOperation, error) {
	operation.UpdatedAt = time.Now()

	dto, err := s.deprovisioningOperationToDTO(&operation)
	if err != nil {
		return nil, fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	lastErr := s.update(dto)
	operation.Version = operation.Version + 1
	return &operation, lastErr
}

// ListDeprovisioningOperationsByInstanceID
func (s *operations) ListDeprovisioningOperationsByInstanceID(instanceID string) ([]internal.DeprovisioningOperation, error) {
	operations, err := s.listOperationsByInstanceIdAndType(instanceID, internal.OperationTypeDeprovision)
	if err != nil {
		return nil, err
	}

	ret, err := s.toDeprovisioningOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// ListDeprovisioningOperations lists deprovisioning operations
func (s *operations) ListDeprovisioningOperations() ([]internal.DeprovisioningOperation, error) {
	var lastErr dberr.Error

	operations, err := s.listOperationsByType(internal.OperationTypeDeprovision)
	if err != nil {
		return nil, lastErr
	}

	ret, err := s.toDeprovisioningOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// InsertUpgradeKymaOperation insert new UpgradeKymaOperation to storage
func (s *operations) InsertUpgradeKymaOperation(operation internal.UpgradeKymaOperation) error {
	dto, err := s.upgradeKymaOperationToDTO(&operation)
	if err != nil {
		return fmt.Errorf("while inserting upgrade kyma operation (id: %s): %w", operation.Operation.ID, err)
	}

	return s.insert(dto)
}

// GetUpgradeKymaOperationByID fetches the UpgradeKymaOperation by given ID, returns error if not found
func (s *operations) GetUpgradeKymaOperationByID(operationID string) (*internal.UpgradeKymaOperation, error) {
	operation, err := s.getByID(operationID)
	if err != nil {
		return nil, fmt.Errorf("while getting operation by ID: %w", err)
	}

	ret, err := s.toUpgradeKymaOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// GetUpgradeKymaOperationByInstanceID fetches the latest UpgradeKymaOperation by given instanceID, returns error if not found
func (s *operations) GetUpgradeKymaOperationByInstanceID(instanceID string) (*internal.UpgradeKymaOperation, error) {
	operation, err := s.getByTypeAndInstanceID(instanceID, internal.OperationTypeUpgradeKyma)
	if err != nil {
		return nil, err
	}
	ret, err := s.toUpgradeKymaOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

func (s *operations) ListUpgradeKymaOperations() ([]internal.UpgradeKymaOperation, error) {
	var operations []dbmodel.OperationDTO

	operations, err := s.listOperationsByType(internal.OperationTypeUpgradeKyma)
	if err != nil {
		return nil, err
	}
	ret, err := s.toUpgradeKymaOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

func (s *operations) ListUpgradeKymaOperationsByInstanceID(instanceID string) ([]internal.UpgradeKymaOperation, error) {
	operations, err := s.listOperationsByInstanceIdAndType(instanceID, internal.OperationTypeUpgradeKyma)
	if err != nil {
		return nil, err
	}

	ret, err := s.toUpgradeKymaOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// UpdateUpgradeKymaOperation updates UpgradeKymaOperation, fails if not exists or optimistic locking failure occurs.
func (s *operations) UpdateUpgradeKymaOperation(operation internal.UpgradeKymaOperation) (*internal.UpgradeKymaOperation, error) {
	operation.UpdatedAt = time.Now()
	dto, err := s.upgradeKymaOperationToDTO(&operation)
	if err != nil {
		return nil, fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	err = s.update(dto)
	operation.Version = operation.Version + 1
	return &operation, err
}

// GetLastOperation returns Operation for given instance ID which is not in 'pending' state. Returns an error if the operation does not exist.
func (s *operations) GetLastOperation(instanceID string) (*internal.Operation, error) {
	session := s.NewReadSession()
	operation := dbmodel.OperationDTO{}
	op := internal.Operation{}
	var lastErr dberr.Error
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operation, lastErr = session.GetLastOperation(instanceID)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("Operation with instance_id %s not exist", instanceID)
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, lastErr
	}
	err = json.Unmarshal([]byte(operation.Data), &op)
	if err != nil {
		return nil, fmt.Errorf("while unmarshalling operation data: %w", err)
	}
	op, err = s.toOperation(&operation, op)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// GetOperationByID returns Operation with given ID. Returns an error if the operation does not exist.
func (s *operations) GetOperationByID(operationID string) (*internal.Operation, error) {
	op := internal.Operation{}
	dto, err := s.getByID(operationID)
	if err != nil {
		return nil, err
	}

	op, err = s.toOperation(dto, op)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal([]byte(dto.Data), &op)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall operation data")
	}
	return &op, nil
}

func (s *operations) GetNotFinishedOperationsByType(operationType internal.OperationType) ([]internal.Operation, error) {
	session := s.NewReadSession()
	operations := make([]dbmodel.OperationDTO, 0)
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		dto, err := session.GetNotFinishedOperationsByType(operationType)
		if err != nil {
			log.Errorf("while getting operations from the storage: %v", err)
			return false, nil
		}
		operations = dto
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return s.toOperations(operations)
}

func (s *operations) GetOperationStatsByPlan() (map[string]internal.OperationStats, error) {
	entries, err := s.NewReadSession().GetOperationStats()
	if err != nil {
		return nil, err
	}
	result := make(map[string]internal.OperationStats)

	for _, entry := range entries {
		if !entry.PlanID.Valid || entry.PlanID.String == "" {
			continue
		}
		planId := entry.PlanID.String
		if _, ok := result[planId]; !ok {
			result[planId] = internal.OperationStats{
				Provisioning:   make(map[domain.LastOperationState]int),
				Deprovisioning: make(map[domain.LastOperationState]int),
			}
		}
		switch internal.OperationType(entry.Type) {
		case internal.OperationTypeProvision:
			result[planId].Provisioning[domain.LastOperationState(entry.State)] += 1
		case internal.OperationTypeDeprovision:
			result[planId].Deprovisioning[domain.LastOperationState(entry.State)] += 1
		}
	}

	return result, nil
}

func calFailedStatusForOrchestration(entries []dbmodel.OperationStatEntry) ([]string, int) {
	var result []string
	resultPerInstanceID := make(map[string][]string)

	for _, entry := range entries {
		resultPerInstanceID[entry.InstanceID] = append(resultPerInstanceID[entry.InstanceID], entry.State)
	}

	var invalidFailed, failedFound bool

	for instanceID, statuses := range resultPerInstanceID {

		invalidFailed = false
		failedFound = false
		for _, status := range statuses {
			if status == Failed {
				failedFound = true
			}
			// In Progress/Retrying/Succeeded means new operation for same instance_id is ongoing/succeeded.
			if status == Succeeded || status == Retrying || status == InProgress {
				invalidFailed = true
			}
		}
		if failedFound && !invalidFailed {
			log.Info("calFailedStatusForOrchestration() append ", instanceID)
			result = append(result, instanceID)
		}
	}

	return result, len(result)
}

func (s *operations) GetOperationStatsForOrchestration(orchestrationID string) (map[string]int, error) {
	entries, err := s.NewReadSession().GetOperationStatsForOrchestration(orchestrationID)
	if err != nil {
		return map[string]int{}, err
	}

	result := make(map[string]int)
	_, failedNum := calFailedStatusForOrchestration(entries)
	result[Failed] = failedNum

	for _, entry := range entries {
		if entry.State != Failed {
			result[entry.State] += 1
		}
	}
	return result, nil
}

func (s *operations) GetOperationsForIDs(operationIDList []string) ([]internal.Operation, error) {
	session := s.NewReadSession()
	operations := make([]dbmodel.OperationDTO, 0)
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		dto, err := session.GetOperationsForIDs(operationIDList)
		if err != nil {
			log.Errorf("while getting operations from the storage: %v", err)
			return false, nil
		}
		operations = dto
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return s.toOperations(operations)
}

func (s *operations) ListOperations(filter dbmodel.OperationFilter) ([]internal.Operation, int, int, error) {
	session := s.NewReadSession()

	var (
		lastErr     error
		size, total int
		operations  = make([]dbmodel.OperationDTO, 0)
	)

	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, size, total, lastErr = session.ListOperations(filter)
		if lastErr != nil {
			log.Errorf("while getting operations from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, -1, -1, err
	}

	result, err := s.toOperations(operations)

	return result, size, total, err
}

func (s *operations) fetchFailedStatusForOrchestration(entries []dbmodel.OperationDTO) ([]dbmodel.OperationDTO, int, int) {
	resPerInstanceID := make(map[string][]dbmodel.OperationDTO)
	for _, entry := range entries {
		resPerInstanceID[entry.InstanceID] = append(resPerInstanceID[entry.InstanceID], entry)
	}

	var failedDatas []dbmodel.OperationDTO
	for _, datas := range resPerInstanceID {
		var invalidFailed bool
		var failedFound bool
		var faildEntry dbmodel.OperationDTO
		for _, data := range datas {
			if data.State == Succeeded || data.State == Retrying || data.State == InProgress {
				invalidFailed = true
				break
			}
			if data.State == Failed {
				failedFound = true
				if faildEntry.InstanceID == "" {
					faildEntry = data
				} else if faildEntry.CreatedAt.Before(data.CreatedAt) {
					faildEntry = data
				}
			}
		}
		if failedFound && !invalidFailed {
			failedDatas = append(failedDatas, faildEntry)
		}
	}
	return failedDatas, len(failedDatas), len(failedDatas)
}

func (s *operations) showUpgradeKymaOperationDTOByOrchestrationID(orchestrationID string, filter dbmodel.OperationFilter) ([]dbmodel.OperationDTO, int, int, error) {
	session := s.NewReadSession()
	var (
		operations        = make([]dbmodel.OperationDTO, 0)
		lastErr           error
		count, totalCount int
	)
	failedFilterFound, _ := s.searchFilter(filter, Failed)
	if failedFilterFound {
		filter.States = []string{}
	}
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, count, totalCount, lastErr = session.ListOperationsByOrchestrationID(orchestrationID, filter)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("Operations for orchestration ID %s not exist", orchestrationID)
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, -1, -1, fmt.Errorf("while getting operation by ID: %w", lastErr)
	}
	if failedFilterFound {
		operations, count, totalCount = s.fetchFailedStatusForOrchestration(operations)
	}
	return operations, count, totalCount, nil
}

func (s *operations) ListUpgradeKymaOperationsByOrchestrationID(orchestrationID string, filter dbmodel.OperationFilter) ([]internal.UpgradeKymaOperation, int, int, error) {
	var (
		operations        = make([]dbmodel.OperationDTO, 0)
		err               error
		count, totalCount int
	)
	states, filterFailedFound := s.excludeFailedInFilterStates(filter, Failed)
	if filterFailedFound {
		filter.States = states
	}

	// excluded "failed" states
	if !filterFailedFound || (filterFailedFound && len(filter.States) > 0) {
		operations, count, totalCount, err = s.showUpgradeKymaOperationDTOByOrchestrationID(orchestrationID, filter)
		if err != nil {
			return nil, -1, -1, fmt.Errorf("while getting operation by ID: %w", err)
		}
	}

	// only for "failed" states
	if filterFailedFound {
		filter = dbmodel.OperationFilter{States: []string{"failed"}}
		failedOperations, failedCount, failedtotalCount, err := s.showUpgradeKymaOperationDTOByOrchestrationID(orchestrationID, filter)
		if err != nil {
			return nil, -1, -1, fmt.Errorf("while getting operation by ID: %w", err)
		}
		operations = append(operations, failedOperations...)
		count = count + failedCount
		totalCount = totalCount + failedtotalCount
	}
	ret, err := s.toUpgradeKymaOperationList(operations)
	if err != nil {
		return nil, -1, -1, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, count, totalCount, nil
}

func (s *operations) ListOperationsByOrchestrationID(orchestrationID string, filter dbmodel.OperationFilter) ([]internal.Operation, int, int, error) {
	session := s.NewReadSession()
	var (
		operations        = make([]dbmodel.OperationDTO, 0)
		lastErr           error
		count, totalCount int
	)
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, count, totalCount, lastErr = session.ListOperationsByOrchestrationID(orchestrationID, filter)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("Operations for orchestration ID %s not exist", orchestrationID)
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, -1, -1, fmt.Errorf("while getting operation by ID: %w", lastErr)
	}
	ret, err := s.toOperationList(operations)
	if err != nil {
		return nil, -1, -1, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, count, totalCount, nil
}

func (s *operations) excludeFailedInFilterStates(filter dbmodel.OperationFilter, state string) ([]string, bool) {
	failedFilterFound, failedFilterIndex := s.searchFilter(filter, state)

	if failedFilterFound {
		filter.States = s.removeIndex(filter.States, failedFilterIndex)
	}
	return filter.States, failedFilterFound
}

func (s *operations) searchFilter(filter dbmodel.OperationFilter, inputState string) (bool, int) {
	var filterFound bool
	var filterIndex int
	for index, state := range filter.States {
		if strings.Contains(state, inputState) {
			filterFound = true
			filterIndex = index
			break
		}
	}
	return filterFound, filterIndex
}

func (s *operations) removeIndex(arr []string, index int) []string {
	return append(arr[:index], arr[index+1:]...)
}

func (s *operations) ListOperationsInTimeRange(from, to time.Time) ([]internal.Operation, error) {
	session := s.NewReadSession()
	operations := make([]dbmodel.OperationDTO, 0)
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		var err error
		operations, err = session.ListOperationsInTimeRange(from, to)
		if err != nil {
			if dberr.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("while listing the operations from the storage: %w", err)
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("while getting operations in range %v-%v: %w", from.Format(time.RFC822), to.Format(time.RFC822), err)
	}
	ret, err := s.toOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

func (s *operations) InsertUpdatingOperation(operation internal.UpdatingOperation) error {
	dto, err := s.updateOperationToDTO(&operation)
	if err != nil {
		return fmt.Errorf("while converting update operation (id: %s): %w", operation.Operation.ID, err)
	}

	return s.insert(dto)
}

func (s *operations) GetUpdatingOperationByID(operationID string) (*internal.UpdatingOperation, error) {
	operation, err := s.getByID(operationID)
	if err != nil {
		return nil, fmt.Errorf("while getting operation by ID: %w", err)
	}

	ret, err := s.toUpdateOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

func (s *operations) UpdateUpdatingOperation(operation internal.UpdatingOperation) (*internal.UpdatingOperation, error) {
	session := s.NewWriteSession()
	operation.UpdatedAt = time.Now()
	dto, err := s.updateOperationToDTO(&operation)
	if err != nil {
		return nil, fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	var lastErr error
	_ = wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		lastErr = session.UpdateOperation(dto)
		if lastErr != nil && dberr.IsNotFound(lastErr) {
			_, lastErr = s.NewReadSession().GetOperationByID(operation.Operation.ID)
			if lastErr != nil {
				log.Errorf("while getting operation: %v", lastErr)
				return false, nil
			}

			// the operation exists but the version is different
			lastErr = dberr.Conflict("operation update conflict, operation ID: %s", operation.Operation.ID)
			log.Warn(lastErr.Error())
			return false, lastErr
		}
		return true, nil
	})
	operation.Version = operation.Version + 1
	return &operation, lastErr
}

// ListUpdatingOperationsByInstanceID Lists all update operations for the given instance
func (s *operations) ListUpdatingOperationsByInstanceID(instanceID string) ([]internal.UpdatingOperation, error) {
	session := s.NewReadSession()
	operations := []dbmodel.OperationDTO{}
	var lastErr dberr.Error
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, lastErr = session.GetOperationsByTypeAndInstanceID(instanceID, internal.OperationTypeUpdate)
		if lastErr != nil {
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, lastErr
	}
	ret, err := s.toUpdateOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// InsertUpgradeClusterOperation insert new UpgradeClusterOperation to storage
func (s *operations) InsertUpgradeClusterOperation(operation internal.UpgradeClusterOperation) error {
	dto, err := s.upgradeClusterOperationToDTO(&operation)
	if err != nil {
		return fmt.Errorf("while converting upgrade cluser operation (id: %s): %w", operation.Operation.ID, err)
	}

	return s.insert(dto)
}

// UpdateUpgradeClusterOperation updates UpgradeClusterOperation, fails if not exists or optimistic locking failure occurs.
func (s *operations) UpdateUpgradeClusterOperation(operation internal.UpgradeClusterOperation) (*internal.UpgradeClusterOperation, error) {
	session := s.NewWriteSession()
	operation.UpdatedAt = time.Now()
	dto, err := s.upgradeClusterOperationToDTO(&operation)
	if err != nil {
		return nil, fmt.Errorf("while converting Operation to DTO: %w", err)
	}

	var lastErr error
	_ = wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		lastErr = session.UpdateOperation(dto)
		if lastErr != nil && dberr.IsNotFound(lastErr) {
			_, lastErr = s.NewReadSession().GetOperationByID(operation.Operation.ID)
			if lastErr != nil {
				log.Errorf("while getting operation: %v", lastErr)
				return false, nil
			}

			// the operation exists but the version is different
			lastErr = dberr.Conflict("operation update conflict, operation ID: %s", operation.Operation.ID)
			log.Warn(lastErr.Error())
			return false, lastErr
		}
		return true, nil
	})
	operation.Version = operation.Version + 1
	return &operation, lastErr
}

// GetUpgradeClusterOperationByID fetches the UpgradeClusterOperation by given ID, returns error if not found
func (s *operations) GetUpgradeClusterOperationByID(operationID string) (*internal.UpgradeClusterOperation, error) {
	operation, err := s.getByID(operationID)
	if err != nil {
		return nil, fmt.Errorf("while getting operation by ID: %w", err)
	}
	ret, err := s.toUpgradeClusterOperation(operation)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// ListUpgradeClusterOperationsByInstanceID Lists all upgrade cluster operations for the given instance
func (s *operations) ListUpgradeClusterOperationsByInstanceID(instanceID string) ([]internal.UpgradeClusterOperation, error) {
	session := s.NewReadSession()
	operations := []dbmodel.OperationDTO{}
	var lastErr dberr.Error
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, lastErr = session.GetOperationsByTypeAndInstanceID(instanceID, internal.OperationTypeUpgradeCluster)
		if lastErr != nil {
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, lastErr
	}
	ret, err := s.toUpgradeClusterOperationList(operations)
	if err != nil {
		return nil, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, nil
}

// ListUpgradeClusterOperationsByOrchestrationID Lists upgrade cluster operations for the given orchestration, according to filter(s) and/or pagination
func (s *operations) ListUpgradeClusterOperationsByOrchestrationID(orchestrationID string, filter dbmodel.OperationFilter) ([]internal.UpgradeClusterOperation, int, int, error) {
	session := s.NewReadSession()
	var (
		operations        = make([]dbmodel.OperationDTO, 0)
		lastErr           error
		count, totalCount int
	)
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, count, totalCount, lastErr = session.ListOperationsByOrchestrationID(orchestrationID, filter)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("Operations for orchestration ID %s not exist", orchestrationID)
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, -1, -1, fmt.Errorf("while getting operation by ID: %w", lastErr)
	}
	ret, err := s.toUpgradeClusterOperationList(operations)
	if err != nil {
		return nil, -1, -1, fmt.Errorf("while converting DTO to Operation: %w", err)
	}

	return ret, count, totalCount, nil
}

func (s *operations) operationToDB(op internal.Operation) (dbmodel.OperationDTO, error) {
	err := s.cipher.EncryptSMCreds(&op.ProvisioningParameters)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while encrypting basic auth: %w", err)
	}
	err = s.cipher.EncryptKubeconfig(&op.ProvisioningParameters)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while encrypting kubeconfig: %w", err)
	}
	pp, err := json.Marshal(op.ProvisioningParameters)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while marshal provisioning parameters: %w", err)
	}

	return dbmodel.OperationDTO{
		ID:                     op.ID,
		Type:                   op.Type,
		TargetOperationID:      op.ProvisionerOperationID,
		State:                  string(op.State),
		Description:            op.Description,
		UpdatedAt:              op.UpdatedAt,
		CreatedAt:              op.CreatedAt,
		Version:                op.Version,
		InstanceID:             op.InstanceID,
		OrchestrationID:        storage.StringToSQLNullString(op.OrchestrationID),
		ProvisioningParameters: storage.StringToSQLNullString(string(pp)),
		FinishedStages:         storage.StringToSQLNullString(strings.Join(op.FinishedStages, ",")),
	}, nil
}

func (s *operations) toOperation(dto *dbmodel.OperationDTO, existingOp internal.Operation) (internal.Operation, error) {
	provisioningParameters := internal.ProvisioningParameters{}
	if dto.ProvisioningParameters.Valid {
		err := json.Unmarshal([]byte(dto.ProvisioningParameters.String), &provisioningParameters)
		if err != nil {
			return internal.Operation{}, fmt.Errorf("while unmarshal provisioning parameters: %w", err)
		}
	}
	err := s.cipher.DecryptSMCreds(&provisioningParameters)
	if err != nil {
		return internal.Operation{}, fmt.Errorf("while decrypting basic auth: %w", err)
	}

	err = s.cipher.DecryptKubeconfig(&provisioningParameters)
	if err != nil {
		log.Warn("decrypting skipped because kubeconfig is in a plain text")
	}

	stages := make([]string, 0)
	finishedSteps := storage.SQLNullStringToString(dto.FinishedStages)
	for _, s := range strings.Split(finishedSteps, ",") {
		if s != "" {
			stages = append(stages, s)
		}
	}

	existingOp.ID = dto.ID
	existingOp.CreatedAt = dto.CreatedAt
	existingOp.UpdatedAt = dto.UpdatedAt
	existingOp.Type = dto.Type
	existingOp.ProvisionerOperationID = dto.TargetOperationID
	existingOp.State = domain.LastOperationState(dto.State)
	existingOp.InstanceID = dto.InstanceID
	existingOp.Description = dto.Description
	existingOp.Version = dto.Version
	existingOp.OrchestrationID = storage.SQLNullStringToString(dto.OrchestrationID)
	existingOp.ProvisioningParameters = provisioningParameters
	existingOp.FinishedStages = stages

	return existingOp, nil
}

func (s *operations) toOperations(op []dbmodel.OperationDTO) ([]internal.Operation, error) {
	operations := make([]internal.Operation, 0)
	for _, o := range op {
		operation := internal.Operation{}
		operation, err := s.toOperation(&o, operation)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal([]byte(o.Data), &operation)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshall operation data: %w", err)
		}
		operations = append(operations, operation)
	}
	return operations, nil
}

func (s *operations) toProvisioningOperation(op *dbmodel.OperationDTO) (*internal.ProvisioningOperation, error) {
	if op.Type != internal.OperationTypeProvision {
		return nil, fmt.Errorf("expected operation type Provisioning, but was %s", op.Type)
	}
	var operation internal.ProvisioningOperation
	var err error
	err = json.Unmarshal([]byte(op.Data), &operation)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall provisioning data: %w", err)
	}
	operation.Operation, err = s.toOperation(op, operation.Operation)
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *operations) toProvisioningOperationList(ops []dbmodel.OperationDTO) ([]internal.ProvisioningOperation, error) {
	result := make([]internal.ProvisioningOperation, 0)

	for _, op := range ops {
		o, err := s.toProvisioningOperation(&op)
		if err != nil {
			return nil, fmt.Errorf("while converting to upgrade kyma operation: %w", err)
		}
		result = append(result, *o)
	}

	return result, nil
}

func (s *operations) toDeprovisioningOperationList(ops []dbmodel.OperationDTO) ([]internal.DeprovisioningOperation, error) {
	result := make([]internal.DeprovisioningOperation, 0)

	for _, op := range ops {
		o, err := s.toDeprovisioningOperation(&op)
		if err != nil {
			return nil, fmt.Errorf("while converting to upgrade kyma operation: %w", err)
		}
		result = append(result, *o)
	}

	return result, nil
}

func (s *operations) operationToDTO(op *internal.Operation) (dbmodel.OperationDTO, error) {
	serialized, err := json.Marshal(op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while serializing operation data %v: %w", op, err)
	}

	ret, err := s.operationToDB(*op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while converting to operationDB %v: %w", op, err)
	}

	ret.Data = string(serialized)
	return ret, nil
}

func (s *operations) provisioningOperationToDTO(op *internal.ProvisioningOperation) (dbmodel.OperationDTO, error) {
	serialized, err := json.Marshal(op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while serializing provisioning data %v: %w", op, err)
	}

	ret, err := s.operationToDB(op.Operation)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while converting to operationDB %v: %w", op, err)
	}
	ret.Data = string(serialized)
	ret.Type = internal.OperationTypeProvision
	return ret, nil
}

func (s *operations) toDeprovisioningOperation(op *dbmodel.OperationDTO) (*internal.DeprovisioningOperation, error) {
	if op.Type != internal.OperationTypeDeprovision {
		return nil, fmt.Errorf(fmt.Sprintf("expected operation type Deprovision, but was %s", op.Type))
	}
	var operation internal.DeprovisioningOperation
	var err error
	err = json.Unmarshal([]byte(op.Data), &operation)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall provisioning data: %w", err)
	}
	operation.Operation, err = s.toOperation(op, operation.Operation)
	if err != nil {
		return nil, err
	}

	return &operation, nil
}

func (s *operations) deprovisioningOperationToDTO(op *internal.DeprovisioningOperation) (dbmodel.OperationDTO, error) {
	serialized, err := json.Marshal(op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while serializing deprovisioning data %v: %w", op, err)
	}

	ret, err := s.operationToDB(op.Operation)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while converting to operationDB %v: %w", op, err)
	}
	ret.Data = string(serialized)
	ret.Type = internal.OperationTypeDeprovision
	return ret, nil
}

func (s *operations) toUpgradeKymaOperation(op *dbmodel.OperationDTO) (*internal.UpgradeKymaOperation, error) {
	if op.Type != internal.OperationTypeUpgradeKyma {
		return nil, fmt.Errorf("expected operation type Upgrade Kyma, but was %s", op.Type)
	}
	var operation internal.UpgradeKymaOperation
	var err error
	err = json.Unmarshal([]byte(op.Data), &operation)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall upgrade kyma data: %w", err)
	}
	operation.Operation, err = s.toOperation(op, operation.Operation)
	if err != nil {
		return nil, err
	}
	operation.RuntimeOperation.ID = op.ID
	if op.OrchestrationID.Valid {
		operation.OrchestrationID = op.OrchestrationID.String
	}

	return &operation, nil
}

func (s *operations) toOperationList(ops []dbmodel.OperationDTO) ([]internal.Operation, error) {
	result := make([]internal.Operation, 0)

	for _, op := range ops {

		var operation internal.Operation
		var err error
		err = json.Unmarshal([]byte(op.Data), &operation)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshall operation data: %w", err)
		}

		o, err := s.toOperation(&op, operation)
		if err != nil {
			return nil, fmt.Errorf("while converting to upgrade kyma operation: %w", err)
		}
		result = append(result, o)
	}

	return result, nil
}

func (s *operations) toUpgradeKymaOperationList(ops []dbmodel.OperationDTO) ([]internal.UpgradeKymaOperation, error) {
	result := make([]internal.UpgradeKymaOperation, 0)

	for _, op := range ops {
		o, err := s.toUpgradeKymaOperation(&op)
		if err != nil {
			return nil, fmt.Errorf("while converting to upgrade kyma operation: %w", err)
		}
		result = append(result, *o)
	}

	return result, nil
}

func (s *operations) upgradeKymaOperationToDTO(op *internal.UpgradeKymaOperation) (dbmodel.OperationDTO, error) {
	serialized, err := json.Marshal(op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while serializing upgrade kyma data %v: %w", op, err)
	}

	ret, err := s.operationToDB(op.Operation)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while converting to operationDB %v: %w", op, err)
	}
	ret.Data = string(serialized)
	ret.Type = internal.OperationTypeUpgradeKyma
	ret.OrchestrationID = storage.StringToSQLNullString(op.OrchestrationID)
	return ret, nil
}

func (s *operations) toUpgradeClusterOperation(op *dbmodel.OperationDTO) (*internal.UpgradeClusterOperation, error) {
	if op.Type != internal.OperationTypeUpgradeCluster {
		return nil, fmt.Errorf("expected operation type upgradeCluster, but was %s", op.Type)
	}
	var operation internal.UpgradeClusterOperation
	var err error
	err = json.Unmarshal([]byte(op.Data), &operation)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall upgrade cluster data: %w", err)
	}
	operation.Operation, err = s.toOperation(op, operation.Operation)
	if err != nil {
		return nil, err
	}
	operation.RuntimeOperation.ID = op.ID
	if op.OrchestrationID.Valid {
		operation.OrchestrationID = op.OrchestrationID.String
	}

	return &operation, nil
}

func (s *operations) toUpgradeClusterOperationList(ops []dbmodel.OperationDTO) ([]internal.UpgradeClusterOperation, error) {
	result := make([]internal.UpgradeClusterOperation, 0)

	for _, op := range ops {
		o, err := s.toUpgradeClusterOperation(&op)
		if err != nil {
			return nil, fmt.Errorf("while converting to upgrade cluster operation: %w", err)
		}
		result = append(result, *o)
	}

	return result, nil
}

func (s *operations) upgradeClusterOperationToDTO(op *internal.UpgradeClusterOperation) (dbmodel.OperationDTO, error) {
	serialized, err := json.Marshal(op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while serializing upgradeCluster data %v: %w", op, err)
	}

	ret, err := s.operationToDB(op.Operation)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while converting to operationDB %v: %w", op, err)
	}
	ret.Data = string(serialized)
	ret.Type = internal.OperationTypeUpgradeCluster
	ret.OrchestrationID = storage.StringToSQLNullString(op.OrchestrationID)
	return ret, nil
}

func (s *operations) updateOperationToDTO(op *internal.UpdatingOperation) (dbmodel.OperationDTO, error) {
	serialized, err := json.Marshal(op)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while serializing update data %v: %w", op, err)
	}

	ret, err := s.operationToDB(op.Operation)
	if err != nil {
		return dbmodel.OperationDTO{}, fmt.Errorf("while converting to operationDB %v: %w", op, err)
	}
	ret.Data = string(serialized)
	ret.Type = internal.OperationTypeUpdate
	ret.OrchestrationID = storage.StringToSQLNullString(op.OrchestrationID)
	return ret, nil
}

func (s *operations) toUpdateOperation(op *dbmodel.OperationDTO) (*internal.UpdatingOperation, error) {
	if op.Type != internal.OperationTypeUpdate {
		return nil, fmt.Errorf(fmt.Sprintf("expected operation type update, but was %s", op.Type))
	}
	var operation internal.UpdatingOperation
	var err error
	err = json.Unmarshal([]byte(op.Data), &operation)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall provisioning data")
	}
	operation.Operation, err = s.toOperation(op, operation.Operation)
	if err != nil {
		return nil, err
	}

	return &operation, nil
}

func (s *operations) toUpdateOperationList(ops []dbmodel.OperationDTO) ([]internal.UpdatingOperation, error) {
	result := make([]internal.UpdatingOperation, 0)

	for _, op := range ops {
		o, err := s.toUpdateOperation(&op)
		if err != nil {
			return nil, fmt.Errorf("while converting to upgrade cluster operation: %w", err)
		}
		result = append(result, *o)
	}

	return result, nil
}

func (s *operations) getByID(id string) (*dbmodel.OperationDTO, error) {
	var lastErr dberr.Error
	session := s.NewReadSession()
	operation := dbmodel.OperationDTO{}

	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operation, lastErr = session.GetOperationByID(id)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("Operation with id %s not exist", id)
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return nil, err
	}

	return &operation, nil
}

func (s *operations) insert(dto dbmodel.OperationDTO) error {
	session := s.NewWriteSession()
	var lastErr error
	_ = wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		lastErr = session.InsertOperation(dto)
		if lastErr != nil {
			log.Errorf("while insert operation: %v", lastErr)
			return false, nil
		}
		// TODO: insert link to orchestration
		return true, nil
	})
	return lastErr
}

func (s *operations) getByInstanceID(id string) (*dbmodel.OperationDTO, error) {
	session := s.NewReadSession()
	operation := dbmodel.OperationDTO{}
	var lastErr dberr.Error
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operation, lastErr = session.GetOperationByInstanceID(id)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("operation does not exist")
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})

	return &operation, err
}

func (s *operations) getByTypeAndInstanceID(id string, opType internal.OperationType) (*dbmodel.OperationDTO, error) {
	session := s.NewReadSession()
	operation := dbmodel.OperationDTO{}
	var lastErr dberr.Error
	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operation, lastErr = session.GetOperationByTypeAndInstanceID(id, opType)
		if lastErr != nil {
			if dberr.IsNotFound(lastErr) {
				lastErr = dberr.NotFound("operation does not exist")
				return false, lastErr
			}
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})

	return &operation, err
}

func (s *operations) update(operation dbmodel.OperationDTO) error {
	session := s.NewWriteSession()

	var lastErr error
	_ = wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		lastErr = session.UpdateOperation(operation)
		if lastErr != nil && dberr.IsNotFound(lastErr) {
			_, lastErr = s.NewReadSession().GetOperationByID(operation.ID)
			if lastErr != nil {
				log.Errorf("while getting operation: %v", lastErr)
				return false, nil
			}

			// the operation exists but the version is different
			lastErr = dberr.Conflict("operation update conflict, operation ID: %s", operation.ID)
			log.Warn(lastErr.Error())
			return false, lastErr
		}
		return true, nil
	})
	return lastErr
}

func (s *operations) listOperationsByInstanceIdAndType(instanceId string, operationType internal.OperationType) ([]dbmodel.OperationDTO, error) {
	session := s.NewReadSession()
	operations := []dbmodel.OperationDTO{}
	var lastErr dberr.Error

	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, lastErr = session.GetOperationsByTypeAndInstanceID(instanceId, operationType)
		if lastErr != nil {
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, lastErr
	}
	return operations, lastErr
}

func (s *operations) listOperationsByType(operationType internal.OperationType) ([]dbmodel.OperationDTO, error) {
	session := s.NewReadSession()
	operations := []dbmodel.OperationDTO{}
	var lastErr dberr.Error

	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, lastErr = session.ListOperationsByType(operationType)
		if lastErr != nil {
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, lastErr
	}
	return operations, lastErr
}

func (s *operations) listOperationsByInstanceId(instanceId string) ([]dbmodel.OperationDTO, error) {
	session := s.NewReadSession()
	operations := []dbmodel.OperationDTO{}
	var lastErr dberr.Error

	err := wait.PollImmediate(defaultRetryInterval, defaultRetryTimeout, func() (bool, error) {
		operations, lastErr = session.GetOperationsByInstanceID(instanceId)
		if lastErr != nil {
			log.Errorf("while reading operation from the storage: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, lastErr
	}
	return operations, lastErr
}

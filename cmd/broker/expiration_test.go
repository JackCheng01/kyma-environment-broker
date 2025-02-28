package main

import (
	"fmt"
	"net/http"
	"testing"

	reconcilerApi "github.com/kyma-incubator/reconciler/pkg/keb"

	"github.com/google/uuid"
	"github.com/kyma-project/kyma-environment-broker/internal"
	"github.com/kyma-project/kyma-environment-broker/internal/broker"
	"github.com/pivotal-cf/brokerapi/v8/domain"
	"github.com/stretchr/testify/assert"
)

const expirationRequestPathFormat = "expire/service_instance/%s"

const trialProvisioningRequestBody = `{
  "service_id": "47c9dcbf-ff30-448e-ab36-d3bad66ba281",
  "plan_id": "7d55d31d-35ae-4438-bf13-6ffdfa107d9f",
  "context": {
    "sm_operator_credentials": {
      "clientid": "sm-operator-client-id",
      "clientsecret": "sm-operator-client-secret",
      "url": "sm-operator-url",
      "sm_url": "sm-operator-url"
    },
    "globalaccount_id": "global-account-id",
    "subaccount_id": "subaccount-id",
    "user_id": "john.smith@email.com"
  },
  "parameters": {
    "name": "trial-test",
    "oidc": {
      "clientID": "client-id",
      "signingAlgs": ["PS512"],
      "issuerURL": "https://issuer.url.com"
    }
  }
}`

const awsProvisioningRequestBody = `{
  "service_id": "47c9dcbf-ff30-448e-ab36-d3bad66ba281",
  "plan_id": "361c511f-f939-4621-b228-d0fb79a1fe15",
  "context": {
    "sm_operator_credentials": {
      "clientid": "sm-operator-client-id",
      "clientsecret": "sm-operator-client-secret",
      "url": "sm-operator-url",
      "sm_url": "sm-operator-url"
    },
    "globalaccount_id": "global-account-id",
    "subaccount_id": "subaccount-id",
    "user_id": "john.smith@email.com"
  },
  "parameters": {
    "name": "aws-test",
    "region": "eu-central-1",
    "oidc": {
      "clientID": "client-id",
      "signingAlgs": ["PS512"],
      "issuerURL": "https://issuer.url.com"
    }
  }
}`

const unsuspensionRequestBody = `{
  "service_id": "47c9dcbf-ff30-448e-ab36-d3bad66ba281",
  "plan_id": "7d55d31d-35ae-4438-bf13-6ffdfa107d9f",
  "context": {
    "globalaccount_id": "global-account-id",
    "subaccount_id": "subaccount-id",
    "user_id": "john.smith@email.com",
    "active": true
  }
}`

const trialDeprovisioningRequestBody = `{
  "service_id": "47c9dcbf-ff30-448e-ab36-d3bad66ba281",
  "plan_id": "7d55d31d-35ae-4438-bf13-6ffdfa107d9f"
}`

func TestExpiration(t *testing.T) {
	t.Run("should expire a trial instance", func(t *testing.T) {
		suite := NewBrokerSuiteTest(t)
		defer suite.TearDown()
		// given
		instanceID := uuid.NewString()
		resp := suite.CallAPI(http.MethodPut,
			fmt.Sprintf(provisioningRequestPathFormat, instanceID),
			trialProvisioningRequestBody)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		provisioningOpID := suite.DecodeOperationID(resp)
		suite.processProvisioningByOperationID(provisioningOpID)
		suite.WaitForOperationState(provisioningOpID, domain.Succeeded)

		// when
		suite.SetReconcilerResponseStatus(reconcilerApi.StatusDeleted)
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		suspensionOpID := suite.DecodeOperationID(resp)
		assert.NotEmpty(t, suspensionOpID)

		suite.WaitForOperationState(suspensionOpID, domain.InProgress)
		suite.FinishDeprovisioningOperationByProvisionerForGivenOpId(suspensionOpID)
		suite.WaitForOperationState(suspensionOpID, domain.Succeeded)

		actualInstance := suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)
	})

	t.Run("should retrigger expiration (suspension) on already expired instance", func(t *testing.T) {
		suite := NewBrokerSuiteTest(t)
		defer suite.TearDown()
		// given
		instanceID := uuid.NewString()
		resp := suite.CallAPI(http.MethodPut,
			fmt.Sprintf(provisioningRequestPathFormat, instanceID),
			trialProvisioningRequestBody)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		provisioningOpID := suite.DecodeOperationID(resp)
		suite.processProvisioningByOperationID(provisioningOpID)
		suite.WaitForOperationState(provisioningOpID, domain.Succeeded)

		// when
		suite.SetReconcilerResponseStatus(reconcilerApi.StatusDeleted)
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		suspensionOpID := suite.DecodeOperationID(resp)
		assert.NotEmpty(t, suspensionOpID)

		suite.WaitForOperationState(suspensionOpID, domain.InProgress)
		suite.FinishDeprovisioningOperationByProvisionerForGivenOpId(suspensionOpID)
		suite.WaitForOperationState(suspensionOpID, domain.Succeeded)

		actualInstance := suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)

		// when
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		suspensionOpID2 := suite.DecodeOperationID(resp)
		assert.NotEmpty(t, suspensionOpID2)
		assert.NotEqual(t, suspensionOpID, suspensionOpID2)

		suite.WaitForOperationState(suspensionOpID2, domain.Succeeded)

		actualInstance = suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)
	})

	t.Run("should expire a trial instance after failed provisioning", func(t *testing.T) {
		suite := NewBrokerSuiteTest(t)
		defer suite.TearDown()
		// given
		instanceID := uuid.NewString()
		resp := suite.CallAPI(http.MethodPut,
			fmt.Sprintf(provisioningRequestPathFormat, instanceID),
			trialProvisioningRequestBody)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		provisioningOpID := suite.DecodeOperationID(resp)
		suite.failProvisioningByOperationID(provisioningOpID)
		suite.WaitForOperationState(provisioningOpID, domain.Failed)

		// when
		suite.SetReconcilerResponseStatus(reconcilerApi.StatusDeleted)
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		suspensionOpID := suite.DecodeOperationID(resp)
		assert.NotEmpty(t, suspensionOpID)

		suite.WaitForOperationState(suspensionOpID, domain.InProgress)
		suite.FinishDeprovisioningOperationByProvisionerForGivenOpId(suspensionOpID)
		suite.WaitForOperationState(suspensionOpID, domain.Succeeded)

		actualInstance := suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)
	})

	t.Run("should expire a trial instance after failed deprovisioning", func(t *testing.T) {
		suite := NewBrokerSuiteTest(t)
		defer suite.TearDown()
		// given
		instanceID := uuid.NewString()
		resp := suite.CallAPI(http.MethodPut,
			fmt.Sprintf(provisioningRequestPathFormat, instanceID),
			trialProvisioningRequestBody)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		provisioningOpID := suite.DecodeOperationID(resp)
		suite.processProvisioningByOperationID(provisioningOpID)
		suite.WaitForOperationState(provisioningOpID, domain.Succeeded)

		suite.SetReconcilerResponseStatus(reconcilerApi.StatusDeleteError)
		resp = suite.CallAPI(http.MethodDelete,
			fmt.Sprintf(deprovisioningRequestPathFormat, instanceID, broker.TrialPlanID),
			trialDeprovisioningRequestBody)

		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		deprovisioningOpID := suite.DecodeOperationID(resp)
		suite.FailDeprovisioningByReconciler(deprovisioningOpID)
		suite.FailDeprovisioningOperationByProvisioner(deprovisioningOpID)
		suite.WaitForOperationState(deprovisioningOpID, domain.Failed)

		// when
		suite.SetReconcilerResponseStatus(reconcilerApi.StatusDeleted)
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		suspensionOpID := suite.DecodeOperationID(resp)
		assert.NotEmpty(t, suspensionOpID)

		suite.WaitForOperationState(suspensionOpID, domain.InProgress)
		suite.FinishDeprovisioningOperationByProvisionerForGivenOpId(suspensionOpID)
		suite.WaitForOperationState(suspensionOpID, domain.Succeeded)

		actualInstance := suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)
	})

	t.Run("should reject an expiration request of non-trial instance", func(t *testing.T) {
		suite := NewBrokerSuiteTest(t)
		defer suite.TearDown()
		// given
		instanceID := uuid.NewString()
		resp := suite.CallAPI(http.MethodPut,
			fmt.Sprintf(provisioningRequestPathFormat, instanceID),
			awsProvisioningRequestBody)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		provisioningOpID := suite.DecodeOperationID(resp)
		suite.processProvisioningByOperationID(provisioningOpID)
		suite.WaitForOperationState(provisioningOpID, domain.Succeeded)

		// when
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		actualInstance := suite.GetInstance(instanceID)
		assert.False(t, actualInstance.IsExpired())
		assert.NotEmpty(t, actualInstance.RuntimeID)
	})

	t.Run("should reject unsuspension request of an expired instance", func(t *testing.T) {
		suite := NewBrokerSuiteTest(t)
		defer suite.TearDown()
		// given
		instanceID := uuid.NewString()
		resp := suite.CallAPI(http.MethodPut,
			fmt.Sprintf(provisioningRequestPathFormat, instanceID),
			trialProvisioningRequestBody)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		provisioningOpID := suite.DecodeOperationID(resp)
		suite.processProvisioningByOperationID(provisioningOpID)
		suite.WaitForOperationState(provisioningOpID, domain.Succeeded)

		// when
		suite.SetReconcilerResponseStatus(reconcilerApi.StatusDeleted)
		resp = suite.CallAPI(http.MethodPut,
			fmt.Sprintf(expirationRequestPathFormat, instanceID),
			"")

		// then
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		suspensionOpID := suite.DecodeOperationID(resp)
		assert.NotEmpty(t, suspensionOpID)

		suite.WaitForOperationState(suspensionOpID, domain.InProgress)
		suite.FinishDeprovisioningOperationByProvisionerForGivenOpId(suspensionOpID)
		suite.WaitForOperationState(suspensionOpID, domain.Succeeded)

		actualInstance := suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)

		// when
		resp = suite.CallAPI(http.MethodPatch,
			fmt.Sprintf(updateRequestPathFormat, instanceID),
			unsuspensionRequestBody)

		// then
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		actualLastOperation := suite.LastOperation(instanceID)
		assert.Equal(t, suspensionOpID, actualLastOperation.ID)
		actualInstance = suite.GetInstance(instanceID)
		assertInstanceIsExpired(t, actualInstance)
	})
}

func assertInstanceIsExpired(t *testing.T, i *internal.Instance) {
	assert.True(t, i.IsExpired())
	assert.False(t, *i.Parameters.ErsContext.Active)
	assert.Empty(t, i.RuntimeID)
}

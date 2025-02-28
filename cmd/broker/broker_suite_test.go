package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/kyma-project/kyma-environment-broker/internal/kubeconfig"

	"code.cloudfoundry.org/lager"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	reconcilerApi "github.com/kyma-incubator/reconciler/pkg/keb"
	"github.com/kyma-project/control-plane/components/provisioner/pkg/gqlschema"
	"github.com/kyma-project/kyma-environment-broker/common/director"
	"github.com/kyma-project/kyma-environment-broker/common/gardener"
	"github.com/kyma-project/kyma-environment-broker/common/orchestration"
	"github.com/kyma-project/kyma-environment-broker/internal"
	"github.com/kyma-project/kyma-environment-broker/internal/avs"
	"github.com/kyma-project/kyma-environment-broker/internal/broker"
	kebConfig "github.com/kyma-project/kyma-environment-broker/internal/config"
	"github.com/kyma-project/kyma-environment-broker/internal/edp"
	"github.com/kyma-project/kyma-environment-broker/internal/event"
	"github.com/kyma-project/kyma-environment-broker/internal/expiration"
	"github.com/kyma-project/kyma-environment-broker/internal/fixture"
	"github.com/kyma-project/kyma-environment-broker/internal/ias"
	"github.com/kyma-project/kyma-environment-broker/internal/notification"
	kebOrchestration "github.com/kyma-project/kyma-environment-broker/internal/orchestration"
	orchestrate "github.com/kyma-project/kyma-environment-broker/internal/orchestration/handlers"
	"github.com/kyma-project/kyma-environment-broker/internal/process"
	"github.com/kyma-project/kyma-environment-broker/internal/process/input"
	"github.com/kyma-project/kyma-environment-broker/internal/process/provisioning"
	"github.com/kyma-project/kyma-environment-broker/internal/process/steps"
	"github.com/kyma-project/kyma-environment-broker/internal/process/update"
	"github.com/kyma-project/kyma-environment-broker/internal/process/upgrade_cluster"
	"github.com/kyma-project/kyma-environment-broker/internal/process/upgrade_kyma"
	"github.com/kyma-project/kyma-environment-broker/internal/provisioner"
	"github.com/kyma-project/kyma-environment-broker/internal/reconciler"
	kebRuntime "github.com/kyma-project/kyma-environment-broker/internal/runtime"
	"github.com/kyma-project/kyma-environment-broker/internal/runtimeoverrides"
	"github.com/kyma-project/kyma-environment-broker/internal/runtimeversion"
	"github.com/kyma-project/kyma-environment-broker/internal/storage"
	"github.com/pivotal-cf/brokerapi/v8/domain"
	"github.com/pivotal-cf/brokerapi/v8/domain/apiresponses"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const fixedGardenerNamespace = "garden-test"

const (
	btpOperatorGroup           = "services.cloud.sap.com"
	btpOperatorApiVer          = "v1"
	btpOperatorServiceInstance = "ServiceInstance"
	btpOperatorServiceBinding  = "ServiceBinding"
	instanceName               = "my-service-instance"
	bindingName                = "my-binding"
	kymaNamespace              = "kyma-system"
)

var (
	serviceBindingGvk = schema.GroupVersionKind{
		Group:   btpOperatorGroup,
		Version: btpOperatorApiVer,
		Kind:    btpOperatorServiceBinding,
	}
	serviceInstanceGvk = schema.GroupVersionKind{
		Group:   btpOperatorGroup,
		Version: btpOperatorApiVer,
		Kind:    btpOperatorServiceInstance,
	}
)

// BrokerSuiteTest is a helper which allows to write simple tests of any KEB processes (provisioning, deprovisioning, update).
// The starting point of a test could be an HTTP call to Broker API.
type BrokerSuiteTest struct {
	db                storage.BrokerStorage
	storageCleanup    func() error
	provisionerClient *provisioner.FakeClient
	directorClient    *director.FakeClient
	reconcilerClient  *reconciler.FakeClient
	gardenerClient    dynamic.Interface

	httpServer *httptest.Server
	router     *mux.Router

	t                   *testing.T
	inputBuilderFactory input.CreatorForPlan

	componentProvider input.ComponentListProvider

	k8sKcp client.Client
	k8sSKR client.Client

	poller broker.Poller
}

type componentProviderDecorated struct {
	componentProvider input.ComponentListProvider
	decorator         map[string]internal.KymaComponent
}

func (s *BrokerSuiteTest) TearDown() {
	s.httpServer.Close()
	if s.storageCleanup != nil {
		err := s.storageCleanup()
		assert.NoError(s.t, err)
	}
}

func NewBrokerSuiteTest(t *testing.T, version ...string) *BrokerSuiteTest {
	cfg := fixConfig()
	return NewBrokerSuiteTestWithConfig(t, cfg, version...)
}

func NewBrokerSuiteTestWithOptionalRegion(t *testing.T, version ...string) *BrokerSuiteTest {
	cfg := fixConfig()
	return NewBrokerSuiteTestWithConfig(t, cfg, version...)
}

func NewBrokerSuiteTestWithConfig(t *testing.T, cfg *Config, version ...string) *BrokerSuiteTest {
	ctx := context.Background()
	sch := internal.NewSchemeForTests()
	apiextensionsv1.AddToScheme(sch)
	additionalKymaVersions := []string{"1.19", "1.20", "main", "2.0"}
	additionalKymaVersions = append(additionalKymaVersions, version...)
	cli := fake.NewClientBuilder().WithScheme(sch).WithRuntimeObjects(fixK8sResources(defaultKymaVer, additionalKymaVersions)...).Build()
	if len(version) == 1 {
		cfg.KymaVersion = version[0] // overriden to
	}

	optionalComponentsDisablers := kebRuntime.ComponentsDisablers{}
	optComponentsSvc := kebRuntime.NewOptionalComponentsService(optionalComponentsDisablers)

	disabledComponentsProvider := kebRuntime.NewDisabledComponentsProvider()

	configProvider := kebConfig.NewConfigProvider(
		kebConfig.NewConfigMapReader(ctx, cli, logrus.New(), defaultKymaVer),
		kebConfig.NewConfigMapKeysValidator(),
		kebConfig.NewConfigMapConverter())

	componentProvider := kebRuntime.NewFakeComponentsProvider()

	inputFactory, err := input.NewInputBuilderFactory(optComponentsSvc, disabledComponentsProvider, componentProvider,
		configProvider, input.Config{
			MachineImageVersion:         "253",
			KubernetesVersion:           "1.18",
			MachineImage:                "coreos",
			URL:                         "http://localhost",
			DefaultGardenerShootPurpose: "testing",
			DefaultTrialProvider:        internal.AWS,
		}, defaultKymaVer, map[string]string{"cf-eu10": "europe", "cf-us10": "us"}, cfg.FreemiumProviders, defaultOIDCValues(), cfg.Broker.IncludeNewMachineTypesInSchema)

	storageCleanup, db, err := GetStorageForE2ETests()
	assert.NoError(t, err)

	require.NoError(t, err)

	logs := logrus.New()
	logs.SetLevel(logrus.DebugLevel)

	gardenerClient := gardener.NewDynamicFakeClient()

	provisionerClient := provisioner.NewFakeClientWithGardener(gardenerClient, "kcp-system")
	eventBroker := event.NewPubSub(logs)

	runtimeOverrides := runtimeoverrides.NewRuntimeOverrides(ctx, cli)
	accountVersionMapping := runtimeversion.NewAccountVersionMapping(ctx, cli, cfg.VersionConfig.Namespace, cfg.VersionConfig.Name, logs)
	runtimeVerConfigurator := runtimeversion.NewRuntimeVersionConfigurator(cfg.KymaVersion, accountVersionMapping, nil)

	directorClient := director.NewFakeClient()
	avsDel, externalEvalCreator, internalEvalAssistant, externalEvalAssistant := createFakeAvsDelegator(t, db, cfg)

	iasFakeClient := ias.NewFakeClient()
	reconcilerClient := reconciler.NewFakeClient()
	bundleBuilder := ias.NewBundleBuilder(iasFakeClient, cfg.IAS)
	edpClient := edp.NewFakeClient()
	accountProvider := fixAccountProvider()
	require.NoError(t, err)

	fakeK8sSKRClient := fake.NewClientBuilder().WithScheme(sch).Build()
	k8sClientProvider := kubeconfig.NewFakeK8sClientProvider(fakeK8sSKRClient)
	provisionManager := process.NewStagedManager(db.Operations(), eventBroker, cfg.OperationTimeout, cfg.Provisioning, logs.WithField("provisioning", "manager"))
	provisioningQueue := NewProvisioningProcessingQueue(context.Background(), provisionManager, workersAmount, cfg, db, provisionerClient, inputFactory,
		avsDel, internalEvalAssistant, externalEvalCreator, runtimeVerConfigurator, runtimeOverrides,
		edpClient, accountProvider, reconcilerClient, k8sClientProvider, cli, logs)

	provisioningQueue.SpeedUp(10000)
	provisionManager.SpeedUp(10000)

	updateManager := process.NewStagedManager(db.Operations(), eventBroker, time.Hour, cfg.Update, logs)
	rvc := runtimeversion.NewRuntimeVersionConfigurator(cfg.KymaVersion, nil, db.RuntimeStates())
	updateQueue := NewUpdateProcessingQueue(context.Background(), updateManager, 1, db, inputFactory, provisionerClient,
		eventBroker, rvc, db.RuntimeStates(), componentProvider, reconcilerClient, *cfg, k8sClientProvider, cli, logs)
	updateQueue.SpeedUp(10000)
	updateManager.SpeedUp(10000)

	deprovisionManager := process.NewStagedManager(db.Operations(), eventBroker, time.Hour, cfg.Deprovisioning, logs.WithField("deprovisioning", "manager"))
	deprovisioningQueue := NewDeprovisioningProcessingQueue(ctx, workersAmount, deprovisionManager, cfg, db, eventBroker,
		provisionerClient, avsDel, internalEvalAssistant, externalEvalAssistant,
		bundleBuilder, edpClient, accountProvider, reconcilerClient, k8sClientProvider, cli, configProvider, logs,
	)
	deprovisionManager.SpeedUp(10000)

	deprovisioningQueue.SpeedUp(10000)

	ts := &BrokerSuiteTest{
		db:                  db,
		storageCleanup:      storageCleanup,
		provisionerClient:   provisionerClient,
		directorClient:      directorClient,
		reconcilerClient:    reconcilerClient,
		gardenerClient:      gardenerClient,
		router:              mux.NewRouter(),
		t:                   t,
		inputBuilderFactory: inputFactory,
		componentProvider:   componentProvider,
		k8sKcp:              cli,
		k8sSKR:              fakeK8sSKRClient,
	}
	ts.poller = &broker.TimerPoller{PollInterval: 3 * time.Millisecond, PollTimeout: 3 * time.Second, Log: ts.t.Log}

	ts.CreateAPI(inputFactory, cfg, db, provisioningQueue, deprovisioningQueue, updateQueue, logs)

	notificationFakeClient := notification.NewFakeClient()
	notificationBundleBuilder := notification.NewBundleBuilder(notificationFakeClient, cfg.Notification)

	upgradeEvaluationManager := avs.NewEvaluationManager(avsDel, avs.Config{})
	runtimeLister := kebOrchestration.NewRuntimeLister(db.Instances(), db.Operations(), kebRuntime.NewConverter(defaultRegion), logs)
	runtimeResolver := orchestration.NewGardenerRuntimeResolver(gardenerClient, fixedGardenerNamespace, runtimeLister, logs)
	kymaQueue := NewKymaOrchestrationProcessingQueue(ctx, db, runtimeOverrides, provisionerClient, eventBroker, inputFactory, &upgrade_kyma.TimeSchedule{
		Retry:              10 * time.Millisecond,
		StatusCheck:        100 * time.Millisecond,
		UpgradeKymaTimeout: 3 * time.Second,
	}, 250*time.Millisecond, runtimeVerConfigurator, runtimeResolver, upgradeEvaluationManager, cfg, avs.NewInternalEvalAssistant(cfg.Avs), reconcilerClient, notificationBundleBuilder, k8sClientProvider, logs, cli, 1000)

	clusterQueue := NewClusterOrchestrationProcessingQueue(ctx, db, provisionerClient, eventBroker, inputFactory, &upgrade_cluster.TimeSchedule{
		Retry:                 10 * time.Millisecond,
		StatusCheck:           100 * time.Millisecond,
		UpgradeClusterTimeout: 3 * time.Second,
	}, 250*time.Millisecond, runtimeResolver, upgradeEvaluationManager, notificationBundleBuilder, logs, cli, *cfg, 1000)

	kymaQueue.SpeedUp(1000)
	clusterQueue.SpeedUp(1000)

	// TODO: in case of cluster upgrade the same Azure Zones must be send to the Provisioner
	orchestrationHandler := orchestrate.NewOrchestrationHandler(db, kymaQueue, clusterQueue, cfg.MaxPaginationPage, logs)
	orchestrationHandler.AttachRoutes(ts.router)

	expirationHandler := expiration.NewHandler(db.Instances(), db.Operations(), deprovisioningQueue, logs)
	expirationHandler.AttachRoutes(ts.router)

	ts.httpServer = httptest.NewServer(ts.router)
	return ts
}

func fakeK8sClientProvider(k8sCli client.Client) func(s string) (client.Client, error) {
	return func(s string) (client.Client, error) {
		return k8sCli, nil
	}
}

func defaultOIDCValues() internal.OIDCConfigDTO {
	return internal.OIDCConfigDTO{
		ClientID:       "client-id-oidc",
		GroupsClaim:    "groups",
		IssuerURL:      "https://issuer.url",
		SigningAlgs:    []string{"RS256"},
		UsernameClaim:  "sub",
		UsernamePrefix: "-",
	}
}

func defaultOIDCConfig() *gqlschema.OIDCConfigInput {
	return &gqlschema.OIDCConfigInput{
		ClientID:       defaultOIDCValues().ClientID,
		GroupsClaim:    defaultOIDCValues().GroupsClaim,
		IssuerURL:      defaultOIDCValues().IssuerURL,
		SigningAlgs:    defaultOIDCValues().SigningAlgs,
		UsernameClaim:  defaultOIDCValues().UsernameClaim,
		UsernamePrefix: defaultOIDCValues().UsernamePrefix,
	}
}

func (s *BrokerSuiteTest) ProcessInfrastructureManagerProvisioningByRuntimeID(runtimeID string) {
	err := s.poller.Invoke(func() (bool, error) {
		gardenerCluster := &unstructured.Unstructured{}
		gardenerCluster.SetGroupVersionKind(steps.GardenerClusterGVK())
		err := s.k8sKcp.Get(context.Background(), client.ObjectKey{
			Namespace: "kyma-system",
			Name:      runtimeID,
		}, gardenerCluster)
		if err != nil {
			return false, nil
		}

		unstructured.SetNestedField(gardenerCluster.Object, "Ready", "status", "state")
		err = s.k8sKcp.Update(context.Background(), gardenerCluster)
		return err == nil, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) ChangeDefaultTrialProvider(provider internal.CloudProvider) {
	s.inputBuilderFactory.(*input.InputBuilderFactory).SetDefaultTrialProvider(provider)
}

func (s *BrokerSuiteTest) CallAPI(method string, path string, body string) *http.Response {
	cli := s.httpServer.Client()
	req, err := http.NewRequest(method, fmt.Sprintf("%s/%s", s.httpServer.URL, path), bytes.NewBuffer([]byte(body)))
	req.Header.Set("X-Broker-API-Version", "2.15")
	require.NoError(s.t, err)

	resp, err := cli.Do(req)
	require.NoError(s.t, err)
	return resp
}

func (s *BrokerSuiteTest) CreateAPI(inputFactory broker.PlanValidator, cfg *Config, db storage.BrokerStorage, provisioningQueue *process.Queue, deprovisionQueue *process.Queue, updateQueue *process.Queue, logs logrus.FieldLogger) {
	servicesConfig := map[string]broker.Service{
		broker.KymaServiceName: {
			Description: "",
			Metadata: broker.ServiceMetadata{
				DisplayName: "kyma",
				SupportUrl:  "https://kyma-project.io",
			},
			Plans: map[string]broker.PlanData{
				broker.AzurePlanID: {
					Description: broker.AzurePlanName,
					Metadata:    broker.PlanMetadata{},
				},
				broker.AWSPlanName: {
					Description: broker.AWSPlanName,
					Metadata:    broker.PlanMetadata{},
				},
				broker.SapConvergedCloudPlanName: {
					Description: broker.SapConvergedCloudPlanName,
					Metadata:    broker.PlanMetadata{},
				},
			},
		},
	}
	planDefaults := func(planID string, platformProvider internal.CloudProvider, provider *internal.CloudProvider) (*gqlschema.ClusterConfigInput, error) {
		return &gqlschema.ClusterConfigInput{}, nil
	}
	createAPI(s.router, servicesConfig, inputFactory, cfg, db, provisioningQueue, deprovisionQueue, updateQueue, lager.NewLogger("api"), logs, planDefaults)

	s.httpServer = httptest.NewServer(s.router)
}

func createFakeAvsDelegator(t *testing.T, db storage.BrokerStorage, cfg *Config) (*avs.Delegator, *provisioning.ExternalEvalCreator, *avs.InternalEvalAssistant, *avs.ExternalEvalAssistant) {
	server := avs.NewMockAvsServer(t)
	mockServer := avs.FixMockAvsServer(server)
	avsConfig := avs.Config{
		OauthTokenEndpoint: fmt.Sprintf("%s/oauth/token", mockServer.URL),
		ApiEndpoint:        fmt.Sprintf("%s/api/v2/evaluationmetadata", mockServer.URL),
	}
	client, err := avs.NewClient(context.TODO(), avsConfig, logrus.New())
	assert.NoError(t, err)
	avsDel := avs.NewDelegator(client, avsConfig, db.Operations())
	externalEvalAssistant := avs.NewExternalEvalAssistant(cfg.Avs)
	internalEvalAssistant := avs.NewInternalEvalAssistant(cfg.Avs)
	externalEvalCreator := provisioning.NewExternalEvalCreator(avsDel, cfg.Avs.Disabled, externalEvalAssistant)

	return avsDel, externalEvalCreator, internalEvalAssistant, externalEvalAssistant
}

func (s *BrokerSuiteTest) CreateProvisionedRuntime(options RuntimeOptions) string {
	randomInstanceId := uuid.New().String()

	instance := fixture.FixInstance(randomInstanceId)
	instance.GlobalAccountID = options.ProvideGlobalAccountID()
	instance.SubAccountID = options.ProvideSubAccountID()
	instance.InstanceDetails.SubAccountID = options.ProvideSubAccountID()
	instance.Parameters.PlatformRegion = options.ProvidePlatformRegion()
	instance.Parameters.Parameters.Region = options.ProvideRegion()
	instance.ProviderRegion = *options.ProvideRegion()

	provisioningOperation := fixture.FixProvisioningOperation(operationID, randomInstanceId)

	require.NoError(s.t, s.db.Instances().Insert(instance))
	require.NoError(s.t, s.db.Operations().InsertOperation(provisioningOperation))

	return instance.InstanceID
}

func (s *BrokerSuiteTest) WaitForProvisioningState(operationID string, state domain.LastOperationState) {
	var op *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, err = s.db.Operations().GetProvisioningOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		return op.State == state, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation expected state %s. The existing operation %+v", state, op)
}

func (s *BrokerSuiteTest) WaitForOperationState(operationID string, state domain.LastOperationState) {
	var op *internal.Operation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, err = s.db.Operations().GetOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		return op.State == state, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation expected state %s != %s. The existing operation %+v", state, op.State, op)
}

func (s *BrokerSuiteTest) WaitForLastOperation(iid string, state domain.LastOperationState) string {
	var op *internal.Operation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, _ = s.db.Operations().GetLastOperation(iid)
		return op.State == state, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation expected state %s. The existing operation %+v", state, op)

	return op.ID
}

func (s *BrokerSuiteTest) LastOperation(iid string) *internal.Operation {
	op, _ := s.db.Operations().GetLastOperation(iid)
	return op
}

func (s *BrokerSuiteTest) FinishProvisioningOperationByProvisionerAndInfrastructureManager(operationID string, operationState gqlschema.OperationState) {
	var op *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, _ = s.db.Operations().GetProvisioningOperationByID(operationID)
		if op.RuntimeID != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation with runtimeID. The existing operation %+v", op)

	s.finishOperationByProvisioner(gqlschema.OperationTypeProvision, operationState, op.RuntimeID)
	if operationState == gqlschema.OperationStateSucceeded {
		s.ProcessInfrastructureManagerProvisioningByRuntimeID(op.RuntimeID)
	}
}

func (s *BrokerSuiteTest) FailProvisioningOperationByProvisioner(operationID string) {
	var op *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, _ = s.db.Operations().GetProvisioningOperationByID(operationID)
		if op.RuntimeID != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation with runtimeID. The existing operation %+v", op)

	s.finishOperationByProvisioner(gqlschema.OperationTypeProvision, gqlschema.OperationStateFailed, op.RuntimeID)
}

func (s *BrokerSuiteTest) FailDeprovisioningOperationByProvisioner(operationID string) {
	var op *internal.DeprovisioningOperation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, _ = s.db.Operations().GetDeprovisioningOperationByID(operationID)
		if op.RuntimeID != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation with runtimeID. The existing operation %+v", op)

	s.finishOperationByProvisioner(gqlschema.OperationTypeDeprovision, gqlschema.OperationStateFailed, op.RuntimeID)
}

func (s *BrokerSuiteTest) FinishDeprovisioningOperationByProvisioner(operationID string) {
	var op *internal.DeprovisioningOperation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, err = s.db.Operations().GetDeprovisioningOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		if op.RuntimeID != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation with runtimeID. The existing operation %+v", op)

	err = s.gardenerClient.Resource(gardener.ShootResource).
		Namespace(fixedGardenerNamespace).
		Delete(context.Background(), op.ShootName, v1.DeleteOptions{})
	require.NoError(s.t, err)

	s.finishOperationByProvisioner(gqlschema.OperationTypeDeprovision, gqlschema.OperationStateSucceeded, op.RuntimeID)
}

func (s *BrokerSuiteTest) FinishUpdatingOperationByProvisioner(operationID string) {
	var op *internal.Operation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, _ = s.db.Operations().GetOperationByID(operationID)
		if op == nil || op.RuntimeID == "" || op.ProvisionerOperationID == "" {
			return false, nil
		}
		return true, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation with runtimeID. The existing operation %+v", op)
	s.finishOperationByOpIDByProvisioner(gqlschema.OperationTypeUpgradeShoot, gqlschema.OperationStateSucceeded, op.ID)
}

func (s *BrokerSuiteTest) FinishDeprovisioningOperationByProvisionerForGivenOpId(operationID string) {
	var op *internal.DeprovisioningOperation
	err := s.poller.Invoke(func() (done bool, err error) {
		op, err = s.db.Operations().GetDeprovisioningOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		if op.RuntimeID != "" && op.ProvisionerOperationID != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for the operation with runtimeID. The existing operation %+v", op)

	uns, err := s.gardenerClient.Resource(gardener.ShootResource).
		Namespace(fixedGardenerNamespace).
		List(context.Background(), v1.ListOptions{})
	require.NoError(s.t, err)
	if len(uns.Items) == 0 {
		s.Log(fmt.Sprintf("shoot %s doesn't exist", op.ShootName))
		s.finishOperationByOpIDByProvisioner(gqlschema.OperationTypeDeprovision, gqlschema.OperationStateSucceeded, op.ID)
		return
	}

	err = s.gardenerClient.Resource(gardener.ShootResource).
		Namespace(fixedGardenerNamespace).
		Delete(context.Background(), op.ShootName, v1.DeleteOptions{})
	require.NoError(s.t, err)

	s.finishOperationByOpIDByProvisioner(gqlschema.OperationTypeDeprovision, gqlschema.OperationStateSucceeded, op.ID)
}

func (s *BrokerSuiteTest) finishOperationByProvisioner(operationType gqlschema.OperationType, state gqlschema.OperationState, runtimeID string) {
	err := s.poller.Invoke(func() (bool, error) {
		status := s.provisionerClient.FindOperationByRuntimeIDAndType(runtimeID, operationType)
		if status.ID != nil {
			s.provisionerClient.FinishProvisionerOperation(*status.ID, state)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for provisioner operation to exist")
}

func (s *BrokerSuiteTest) finishOperationByOpIDByProvisioner(operationType gqlschema.OperationType, state gqlschema.OperationState, operationID string) {
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetOperationByID(operationID)
		if err != nil {
			s.Log(fmt.Sprintf("failed to GetOperationsByID: %v", err))
			return false, nil
		}
		status, err := s.provisionerClient.RuntimeOperationStatus("", op.ProvisionerOperationID)
		if err != nil {
			s.Log(fmt.Sprintf("failed to get RuntimeOperationStatus: %v", err))
			return false, nil
		}
		if status.Operation != operationType {
			s.Log(fmt.Sprintf("operation types don't match, expected: %s, actual: %s", operationType.String(), status.Operation.String()))
			return false, nil
		}
		if status.ID != nil {
			s.provisionerClient.FinishProvisionerOperation(*status.ID, state)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err, "timeout waiting for provisioner operation to exist")
}

func (s *BrokerSuiteTest) MarkClusterConfigurationDeleted(iid string) {
	op, _ := s.db.Operations().GetDeprovisioningOperationByInstanceID(iid)
	s.reconcilerClient.ChangeClusterState(op.RuntimeID, op.ClusterConfigurationVersion, reconcilerApi.StatusDeleted)
}

func (s *BrokerSuiteTest) RemoveFromReconcilerByInstanceID(iid string) {
	op, _ := s.db.Operations().GetDeprovisioningOperationByInstanceID(iid)
	s.reconcilerClient.DeleteCluster(op.RuntimeID)
}

func (s *BrokerSuiteTest) FinishProvisioningOperationByReconciler(operationID string) {
	// wait until ProvisioningOperation reaches CreateRuntime step
	var provisioningOp *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetProvisioningOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		if op.ProvisionerOperationID != "" || broker.IsOwnClusterPlan(op.ProvisioningParameters.PlanID) {
			provisioningOp = op
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)

	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		state, err = s.reconcilerClient.GetCluster(provisioningOp.RuntimeID, provisioningOp.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		if state.Cluster != "" {
			s.reconcilerClient.ChangeClusterState(provisioningOp.RuntimeID, provisioningOp.ClusterConfigurationVersion, reconcilerApi.StatusReady)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
}

func (c *BrokerSuiteTest) SetReconcilerResponseStatus(s reconcilerApi.Status) {
	c.reconcilerClient.PrepareReconcilerClusterStatus(s)
}

func (s *BrokerSuiteTest) FailProvisioningOperationByReconciler(operationID string) {
	// wait until ProvisioningOperation reaches CreateRuntime step
	var provisioningOp *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetProvisioningOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		if op.ProvisionerOperationID != "" || broker.IsOwnClusterPlan(op.ProvisioningParameters.PlanID) {
			provisioningOp = op
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)

	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		state, err = s.reconcilerClient.GetCluster(provisioningOp.RuntimeID, provisioningOp.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		if state.Cluster != "" {
			s.reconcilerClient.ChangeClusterState(provisioningOp.RuntimeID, provisioningOp.ClusterConfigurationVersion, reconcilerApi.StatusError)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) FinishReconciliation(opID string) {
	var state *reconcilerApi.HTTPClusterResponse
	err := s.poller.Invoke(func() (bool, error) {
		provisioningOp, err := s.db.Operations().GetProvisioningOperationByID(opID)
		if err != nil {
			return false, nil
		}
		state, err = s.reconcilerClient.GetCluster(provisioningOp.RuntimeID, provisioningOp.ClusterConfigurationVersion)
		if err != nil {
			return false, nil
		}
		if state.Cluster != "" {
			s.reconcilerClient.ChangeClusterState(provisioningOp.RuntimeID, provisioningOp.ClusterConfigurationVersion, reconcilerApi.StatusReady)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) FinishDeprovisioningByReconciler(opID string) {

	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetDeprovisioningOperationByID(opID)
		if err != nil {
			return false, nil
		}
		_, err = s.reconcilerClient.GetCluster(op.RuntimeID, op.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		s.reconcilerClient.ChangeClusterState(op.RuntimeID, op.ClusterConfigurationVersion, reconcilerApi.StatusDeleted)
		return true, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) FailDeprovisioningByReconciler(opID string) {

	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetDeprovisioningOperationByID(opID)
		if err != nil {
			return false, nil
		}
		_, err = s.reconcilerClient.GetCluster(op.RuntimeID, op.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		s.reconcilerClient.ChangeClusterState(op.RuntimeID, op.ClusterConfigurationVersion, reconcilerApi.StatusDeleteError)
		return true, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) FinishUpdatingOperationByReconciler(operationID string) {
	op, err := s.db.Operations().GetOperationByID(operationID)
	assert.NoError(s.t, err)
	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		state, err = s.reconcilerClient.GetCluster(op.RuntimeID, op.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		if state.Cluster != "" {
			s.reconcilerClient.ChangeClusterState(op.RuntimeID, op.ClusterConfigurationVersion, reconcilerApi.StatusReady)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) AssertProvisionerStartedProvisioning(operationID string) {
	// wait until ProvisioningOperation reaches CreateRuntime step
	var provisioningOp *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetProvisioningOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		if op.ProvisionerOperationID != "" {
			provisioningOp = op
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
	require.NotNil(s.t, provisioningOp, "Provisioning operation should not be nil")

	var status gqlschema.OperationStatus
	err = s.poller.Invoke(func() (bool, error) {
		status = s.provisionerClient.FindOperationByRuntimeIDAndType(provisioningOp.RuntimeID, gqlschema.OperationTypeProvision)
		if status.ID != nil {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
	assert.Equal(s.t, gqlschema.OperationStateInProgress, status.State)
}

func (s *BrokerSuiteTest) FinishUpgradeKymaOperationByReconciler(operationID string) {
	var upgradeOp *internal.UpgradeKymaOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetUpgradeKymaOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		if op.ClusterConfigurationVersion != 0 {
			upgradeOp = op
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)

	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		state, err = s.reconcilerClient.GetCluster(upgradeOp.InstanceDetails.RuntimeID, upgradeOp.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		if state.Cluster != "" {
			s.reconcilerClient.ChangeClusterState(upgradeOp.InstanceDetails.RuntimeID, upgradeOp.ClusterConfigurationVersion, reconcilerApi.StatusReady)
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) FinishUpgradeClusterOperationByProvisioner(operationID string) {
	var upgradeOp *internal.UpgradeClusterOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetUpgradeClusterOperationByID(operationID)
		if err != nil {
			return false, nil
		}
		upgradeOp = op
		return true, nil
	})
	assert.NoError(s.t, err)

	s.finishOperationByOpIDByProvisioner(gqlschema.OperationTypeUpgradeShoot, gqlschema.OperationStateSucceeded, upgradeOp.Operation.ID)
}

func (s *BrokerSuiteTest) AssertReconcilerStartedReconcilingWhenProvisioning(provisioningOpID string) {
	var provisioningOp *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetProvisioningOperationByID(provisioningOpID)
		if err != nil {
			return false, nil
		}
		if op.ProvisionerOperationID != "" || broker.IsOwnClusterPlan(op.ProvisioningParameters.PlanID) {
			provisioningOp = op
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)

	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		state, err = s.reconcilerClient.GetCluster(provisioningOp.RuntimeID, 1)
		if state.Cluster != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
	assert.Equal(s.t, reconcilerApi.StatusReconcilePending, state.Status)
}

func (s *BrokerSuiteTest) AssertReconcilerStartedReconcilingWhenUpgrading(opID string) {
	// wait until UpgradeOperation reaches Apply_Cluster_Configuration step
	var upgradeKymaOp *internal.UpgradeKymaOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetUpgradeKymaOperationByID(opID)
		upgradeKymaOp = op
		return err == nil && op != nil, nil
	})
	assert.NoError(s.t, err)

	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		fmt.Println(upgradeKymaOp)
		state, err := s.reconcilerClient.GetCluster(upgradeKymaOp.InstanceDetails.RuntimeID, upgradeKymaOp.InstanceDetails.ClusterConfigurationVersion)
		if err != nil {
			return false, err
		}
		if state.Cluster != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
	assert.Equal(s.t, reconcilerApi.StatusReconcilePending, state.Status)
}

func (s *BrokerSuiteTest) DecodeErrorResponse(resp *http.Response) apiresponses.ErrorResponse {
	m, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	require.NoError(s.t, err)

	r := apiresponses.ErrorResponse{}
	err = json.Unmarshal(m, &r)
	require.NoError(s.t, err)

	return r
}

func (s *BrokerSuiteTest) DecodeOperationID(resp *http.Response) string {
	m, err := io.ReadAll(resp.Body)
	s.Log(string(m))
	require.NoError(s.t, err)
	var provisioningResp struct {
		Operation string `json:"operation"`
	}
	err = json.Unmarshal(m, &provisioningResp)
	require.NoError(s.t, err)

	return provisioningResp.Operation
}

func (s *BrokerSuiteTest) DecodeOrchestrationID(resp *http.Response) string {
	m, err := io.ReadAll(resp.Body)
	s.Log(string(m))
	require.NoError(s.t, err)
	var upgradeResponse orchestration.UpgradeResponse
	err = json.Unmarshal(m, &upgradeResponse)
	require.NoError(s.t, err)

	return upgradeResponse.OrchestrationID
}

func (s *BrokerSuiteTest) DecodeLastUpgradeKymaOperationFromOrchestration(resp *http.Response) (*orchestration.OperationResponse, error) {
	m, err := io.ReadAll(resp.Body)
	s.Log(string(m))
	require.NoError(s.t, err)
	var operationsList orchestration.OperationResponseList
	err = json.Unmarshal(m, &operationsList)
	require.NoError(s.t, err)

	if operationsList.TotalCount == 0 || len(operationsList.Data) == 0 {
		return nil, errors.New("no operations found for given orchestration")
	}

	return &operationsList.Data[len(operationsList.Data)-1], nil
}

func (s *BrokerSuiteTest) DecodeLastUpgradeKymaOperationIDFromOrchestration(resp *http.Response) (string, error) {
	operation, err := s.DecodeLastUpgradeKymaOperationFromOrchestration(resp)
	if err == nil {
		return operation.OperationID, err
	} else {
		return "", err
	}
}

func (s *BrokerSuiteTest) DecodeLastUpgradeClusterOperationIDFromOrchestration(orchestrationID string) (string, error) {
	var operationsList orchestration.OperationResponseList
	err := s.poller.Invoke(func() (bool, error) {
		resp := s.CallAPI("GET", fmt.Sprintf("orchestrations/%s/operations", orchestrationID), "")
		m, err := io.ReadAll(resp.Body)
		s.Log(string(m))
		if err != nil {
			return false, fmt.Errorf("failed to read response body: %v", err)
		}
		operationsList = orchestration.OperationResponseList{}
		err = json.Unmarshal(m, &operationsList)
		if err != nil {
			return false, fmt.Errorf("failed to marshal: %v", err)
		}
		if operationsList.TotalCount == 0 || len(operationsList.Data) == 0 {
			return false, nil
		}
		return true, nil
	})
	require.NoError(s.t, err)
	if operationsList.TotalCount == 0 || len(operationsList.Data) == 0 {
		return "", errors.New("no operations found for given orchestration")
	}

	return operationsList.Data[len(operationsList.Data)-1].OperationID, nil
}

func (s *BrokerSuiteTest) AssertShootUpgrade(operationID string, config gqlschema.UpgradeShootInput) {
	// wait until the operation reaches the call to a Provisioner (provisioner operation ID is stored)
	var provisioningOp *internal.Operation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetOperationByID(operationID)
		assert.NoError(s.t, err)
		if op.ProvisionerOperationID != "" || broker.IsOwnClusterPlan(op.ProvisioningParameters.PlanID) {
			provisioningOp = op
			return true, nil
		}
		return false, nil
	})
	require.NoError(s.t, err)

	var shootUpgrade gqlschema.UpgradeShootInput
	var found bool
	err = s.poller.Invoke(func() (bool, error) {
		shootUpgrade, found = s.provisionerClient.LastShootUpgrade(provisioningOp.RuntimeID)
		if found {
			return true, nil
		}
		return false, nil
	})
	require.NoError(s.t, err)

	assert.Equal(s.t, config, shootUpgrade)
}

func (s *BrokerSuiteTest) AssertInstanceRuntimeAdmins(instanceId string, expectedAdmins []string) {
	var instance *internal.Instance
	err := s.poller.Invoke(func() (bool, error) {
		instance = s.GetInstance(instanceId)
		if instance != nil {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
	assert.Equal(s.t, expectedAdmins, instance.Parameters.Parameters.RuntimeAdministrators)
}

func (s *BrokerSuiteTest) fetchProvisionInput() gqlschema.ProvisionRuntimeInput {
	input := s.provisionerClient.GetLatestProvisionRuntimeInput()
	return input
}

func (s *BrokerSuiteTest) AssertProvider(expectedProvider string) {
	input := s.fetchProvisionInput()
	assert.Equal(s.t, expectedProvider, input.ClusterConfig.GardenerConfig.Provider)
}

func (s *BrokerSuiteTest) AssertProvisionRuntimeInputWithoutKymaConfig() {
	input := s.fetchProvisionInput()
	assert.Nil(s.t, input.KymaConfig)
}

func (s *BrokerSuiteTest) AssertClusterState(operationID string, expectedState reconcilerApi.HTTPClusterResponse) {
	var provisioningOp *internal.ProvisioningOperation
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetProvisioningOperationByID(operationID)
		assert.NoError(s.t, err)
		if op.ProvisionerOperationID != "" {
			provisioningOp = op
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)

	var state *reconcilerApi.HTTPClusterResponse
	err = s.poller.Invoke(func() (bool, error) {
		state, err = s.reconcilerClient.GetLatestCluster(provisioningOp.RuntimeID)
		if err == nil {
			return true, nil
		}
		return false, err
	})
	assert.NoError(s.t, err)

	assert.Equal(s.t, expectedState, state)
}

func (s *BrokerSuiteTest) AssertClusterConfig(operationID string, expectedClusterConfig *reconcilerApi.Cluster) {
	clusterConfig := s.getClusterConfig(operationID)

	assert.Equal(s.t, *expectedClusterConfig, clusterConfig)
}

func (s *BrokerSuiteTest) AssertClusterKymaConfig(operationID string, expectedKymaConfig reconcilerApi.KymaConfig) {
	clusterConfig := s.getClusterConfig(operationID)

	// values in arrays need to be sorted, because globalOverrides are coming from a map and map's elements' order is not deterministic
	for _, component := range clusterConfig.KymaConfig.Components {
		sort.Slice(component.Configuration, func(i, j int) bool {
			return component.Configuration[i].Key < component.Configuration[j].Key
		})
	}
	for _, component := range expectedKymaConfig.Components {
		sort.Slice(component.Configuration, func(i, j int) bool {
			return component.Configuration[i].Key < component.Configuration[j].Key
		})
	}

	assert.Equal(s.t, expectedKymaConfig, clusterConfig.KymaConfig)
}

func (s *BrokerSuiteTest) AssertComponent(a, b reconcilerApi.Component) {
	sort.Slice(a.Configuration, func(i, j int) bool { return a.Configuration[i].Key < a.Configuration[j].Key })
	sort.Slice(b.Configuration, func(i, j int) bool { return b.Configuration[i].Key < b.Configuration[j].Key })
	assert.Equal(s.t, a, b)
}

func (s *BrokerSuiteTest) AssertClusterConfigWithKubeconfig(id string) {
	clusterConfig := s.getClusterConfig(id)

	assert.NotEmpty(s.t, clusterConfig.Kubeconfig)
}

func (s *BrokerSuiteTest) AssertClusterMetadata(id string, metadata reconcilerApi.Metadata) {
	clusterConfig := s.getClusterConfig(id)

	assert.Equal(s.t, metadata, clusterConfig.Metadata)
}

func (s *BrokerSuiteTest) AssertDisabledNetworkFilterForProvisioning(val *bool) {
	var got, exp string
	err := s.poller.Invoke(func() (bool, error) {
		input := s.provisionerClient.GetLatestProvisionRuntimeInput()
		gc := input.ClusterConfig.GardenerConfig
		if reflect.DeepEqual(val, gc.ShootNetworkingFilterDisabled) {
			return true, nil
		}
		got = "<nil>"
		if gc.ShootNetworkingFilterDisabled != nil {
			got = fmt.Sprintf("%v", *gc.ShootNetworkingFilterDisabled)
		}
		exp = "<nil>"
		if val != nil {
			exp = fmt.Sprintf("%v", *val)
		}
		return false, nil
	})
	if err != nil {
		err = fmt.Errorf("ShootNetworkingFilterDisabled expected %v, got %v", exp, got)
	}
	require.NoError(s.t, err)
}

func (s *BrokerSuiteTest) AssertDisabledNetworkFilterRuntimeState(runtimeid, op string, val *bool) {
	var got, exp string
	err := s.poller.Invoke(func() (bool, error) {
		states, _ := s.db.RuntimeStates().ListByRuntimeID(runtimeid)
		exp = "<nil>"
		if val != nil {
			exp = fmt.Sprintf("%v", *val)
		}
		for _, rs := range states {
			if rs.OperationID != op {
				// skip runtime states for different operations
				continue
			}
			if rs.ClusterSetup != nil {
				// skip reconciler runtime states, the test is interested only in provisioner rs
				continue
			}
			if reflect.DeepEqual(val, rs.ClusterConfig.ShootNetworkingFilterDisabled) {
				return true, nil
			}
			got = "<nil>"
			if rs.ClusterConfig.ShootNetworkingFilterDisabled != nil {
				got = fmt.Sprintf("%v", *rs.ClusterConfig.ShootNetworkingFilterDisabled)
			}
			return false, nil
		}
		return false, nil
	})
	if err != nil {
		err = fmt.Errorf("ShootNetworkingFilterDisabled expected %v, got %v", exp, got)
	}
	require.NoError(s.t, err)
}

func (s *BrokerSuiteTest) getClusterConfig(operationID string) reconcilerApi.Cluster {
	provisioningOp, err := s.db.Operations().GetProvisioningOperationByID(operationID)
	assert.NoError(s.t, err)

	var clusterConfig *reconcilerApi.Cluster
	err = s.poller.Invoke(func() (bool, error) {
		clusterConfig, err = s.reconcilerClient.LastClusterConfig(provisioningOp.RuntimeID)
		if err != nil {
			return false, err
		}
		if clusterConfig.RuntimeID != "" {
			return true, nil
		}
		return false, nil
	})
	require.NoError(s.t, err)

	return *clusterConfig
}

func (s *BrokerSuiteTest) LastProvisionInput(iid string) gqlschema.ProvisionRuntimeInput {
	// wait until the operation reaches the call to a Provisioner (provisioner operation ID is stored)
	err := s.poller.Invoke(func() (bool, error) {
		op, err := s.db.Operations().GetProvisioningOperationByInstanceID(iid)
		assert.NoError(s.t, err)
		if op.ProvisionerOperationID != "" {
			return true, nil
		}
		return false, nil
	})
	assert.NoError(s.t, err)
	return s.provisionerClient.LastProvisioning()
}

func (s *BrokerSuiteTest) Log(msg string) {
	s.t.Log(msg)
}

func (s *BrokerSuiteTest) EnableDumpingProvisionerRequests() {
	s.provisionerClient.EnableRequestDumping()
}

func (s *BrokerSuiteTest) GetInstance(iid string) *internal.Instance {
	inst, err := s.db.Instances().GetByID(iid)
	require.NoError(s.t, err)
	return inst
}

func (s *BrokerSuiteTest) processProvisioningByOperationID(opID string) {
	s.WaitForProvisioningState(opID, domain.InProgress)
	s.AssertProvisionerStartedProvisioning(opID)

	s.FinishProvisioningOperationByProvisionerAndInfrastructureManager(opID, gqlschema.OperationStateSucceeded)
	_, err := s.gardenerClient.Resource(gardener.ShootResource).Namespace(fixedGardenerNamespace).Create(context.Background(), s.fixGardenerShootForOperationID(opID), v1.CreateOptions{})
	require.NoError(s.t, err)

	// provisioner finishes the operation
	s.WaitForOperationState(opID, domain.Succeeded)
}

func (s *BrokerSuiteTest) processUpdatingByOperationID(opID string) {
	s.WaitForProvisioningState(opID, domain.InProgress)

	s.FinishUpdatingOperationByProvisioner(opID)

	// provisioner finishes the operation
	s.WaitForOperationState(opID, domain.Succeeded)
}

func (s *BrokerSuiteTest) failProvisioningByOperationID(opID string) {
	s.WaitForProvisioningState(opID, domain.InProgress)
	s.AssertProvisionerStartedProvisioning(opID)

	s.FinishProvisioningOperationByProvisionerAndInfrastructureManager(opID, gqlschema.OperationStateFailed)

	// provisioner finishes the operation
	s.WaitForOperationState(opID, domain.Failed)
}

func (s *BrokerSuiteTest) fixGardenerShootForOperationID(opID string) *unstructured.Unstructured {
	op, err := s.db.Operations().GetProvisioningOperationByID(opID)
	require.NoError(s.t, err)

	un := unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":      op.ShootName,
				"namespace": fixedGardenerNamespace,
				"labels": map[string]interface{}{
					globalAccountLabel: op.ProvisioningParameters.ErsContext.GlobalAccountID,
					subAccountLabel:    op.ProvisioningParameters.ErsContext.SubAccountID,
				},
				"annotations": map[string]interface{}{
					runtimeIDAnnotation: op.RuntimeID,
				},
			},
			"spec": map[string]interface{}{
				"region": "eu",
				"maintenance": map[string]interface{}{
					"timeWindow": map[string]interface{}{
						"begin": "030000+0000",
						"end":   "040000+0000",
					},
				},
			},
		},
	}
	un.SetGroupVersionKind(shootGVK)
	return &un
}

func (s *BrokerSuiteTest) processReconcilingByOperationID(opID string) {
	// Reconciler part
	s.AssertReconcilerStartedReconcilingWhenProvisioning(opID)
	s.FinishProvisioningOperationByReconciler(opID)

	s.WaitForOperationState(opID, domain.Succeeded)
}

func (s *BrokerSuiteTest) processProvisioningByInstanceID(iid string) {
	opID := s.WaitForLastOperation(iid, domain.InProgress)

	s.processProvisioningByOperationID(opID)
}

func (s *BrokerSuiteTest) ShootName(id string) string {
	op, err := s.db.Operations().GetProvisioningOperationByID(id)
	require.NoError(s.t, err)
	return op.ShootName
}

func (s *BrokerSuiteTest) AssertAWSRegionAndZone(region string) {
	input := s.provisionerClient.GetLatestProvisionRuntimeInput()
	assert.Equal(s.t, region, input.ClusterConfig.GardenerConfig.Region)
	assert.Contains(s.t, input.ClusterConfig.GardenerConfig.ProviderSpecificConfig.AwsConfig.AwsZones[0].Name, region)
}

func (s *BrokerSuiteTest) AssertAzureRegion(region string) {
	input := s.provisionerClient.GetLatestProvisionRuntimeInput()
	assert.Equal(s.t, region, input.ClusterConfig.GardenerConfig.Region)
}

// fixExpectedComponentListWithoutSMProxy provides a fixed components list for Service Management without BTP operator credentials provided
func (s *BrokerSuiteTest) fixExpectedComponentListWithoutSMProxy(opID string) []reconcilerApi.Component {
	return []reconcilerApi.Component{
		{
			URL:       "",
			Component: "ory",
			Namespace: "kyma-system",
			Configuration: []reconcilerApi.Configuration{
				{
					Key:    "global.domainName",
					Value:  fmt.Sprintf("%s.kyma.sap.com", s.ShootName(opID)),
					Secret: false,
				},
				{
					Key:    "foo",
					Value:  "bar",
					Secret: false,
				},
				{
					Key:    "global.booleanOverride.enabled",
					Value:  false,
					Secret: false,
				},
			},
		},
		{
			URL:       "",
			Component: "monitoring",
			Namespace: "kyma-system",
			Configuration: []reconcilerApi.Configuration{
				{
					Key:    "global.domainName",
					Value:  fmt.Sprintf("%s.kyma.sap.com", s.ShootName(opID)),
					Secret: false,
				},
				{
					Key:    "foo",
					Value:  "bar",
					Secret: false,
				},
				{
					Key:    "global.booleanOverride.enabled",
					Value:  false,
					Secret: false,
				},
			},
		},
	}
}

// fixExpectedComponentListWithSMOperator provides a fixed components list for Service Management 2.0 - when `sm_operator_credentials`
// object is provided: btp-opeartor component should be installed
func (s *BrokerSuiteTest) fixExpectedComponentListWithSMOperator(opID, smClusterID string) []reconcilerApi.Component {
	return []reconcilerApi.Component{
		{
			URL:       "",
			Component: "cluster-essentials",
			Namespace: "kyma-system",
			Configuration: []reconcilerApi.Configuration{
				{
					Key:    "global.domainName",
					Value:  fmt.Sprintf("%s.kyma.sap.com", s.ShootName(opID)),
					Secret: false,
				},
				{
					Key:    "foo",
					Value:  "bar",
					Secret: false,
				},
				{
					Key:    "global.booleanOverride.enabled",
					Value:  false,
					Secret: false,
				},
			},
		},
		{
			URL:       "",
			Component: "ory",
			Namespace: "kyma-system",
			Configuration: []reconcilerApi.Configuration{
				{
					Key:    "global.domainName",
					Value:  fmt.Sprintf("%s.kyma.sap.com", s.ShootName(opID)),
					Secret: false,
				},
				{
					Key:    "foo",
					Value:  "bar",
					Secret: false,
				},
				{
					Key:    "global.booleanOverride.enabled",
					Value:  false,
					Secret: false,
				},
			},
		},
		{
			URL:       "",
			Component: "monitoring",
			Namespace: "kyma-system",
			Configuration: []reconcilerApi.Configuration{
				{
					Key:    "global.domainName",
					Value:  fmt.Sprintf("%s.kyma.sap.com", s.ShootName(opID)),
					Secret: false,
				},
				{
					Key:    "foo",
					Value:  "bar",
					Secret: false,
				},
				{
					Key:    "global.booleanOverride.enabled",
					Value:  false,
					Secret: false,
				},
			},
		},
		{
			URL:       "https://btp-operator",
			Component: "btp-operator",
			Namespace: "kyma-system",
			Configuration: []reconcilerApi.Configuration{
				{
					Key:    "global.domainName",
					Value:  fmt.Sprintf("%s.kyma.sap.com", s.ShootName(opID)),
					Secret: false,
				},
				{
					Key:    "foo",
					Value:  "bar",
					Secret: false,
				},
				{
					Key:    "global.booleanOverride.enabled",
					Value:  false,
					Secret: false,
				},
				{
					Key:    "manager.secret.clientid",
					Value:  "testClientID",
					Secret: true,
				},
				{
					Key:    "manager.secret.clientsecret",
					Value:  "testClientSecret",
					Secret: true,
				},
				{
					Key:    "manager.secret.url",
					Value:  "https://service-manager.kyma.com",
					Secret: false,
				},
				{
					Key:    "manager.secret.sm_url",
					Value:  "https://service-manager.kyma.com",
					Secret: false,
				},
				{
					Key:    "manager.secret.tokenurl",
					Value:  "https://test.auth.com",
					Secret: false,
				},
				{
					Key:    "cluster.id",
					Value:  smClusterID,
					Secret: false,
				},
				{
					Key:   "manager.priorityClassName",
					Value: "kyma-system",
				},
			},
		},
	}
}

func (s *BrokerSuiteTest) AssertKymaResourceExists(opId string) {
	operation, err := s.db.Operations().GetOperationByID(opId)
	assert.NoError(s.t, err)

	obj := &unstructured.Unstructured{}
	obj.SetName(operation.RuntimeID)
	obj.SetNamespace("kyma-system")
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.kyma-project.io",
		Version: "v1beta2",
		Kind:    "Kyma",
	})

	err = s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)

	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) AssertKymaResourceExistsByInstanceID(instanceID string) {
	instance := s.GetInstance(instanceID)

	obj := &unstructured.Unstructured{}
	obj.SetName(instance.RuntimeID)
	obj.SetNamespace("kyma-system")
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.kyma-project.io",
		Version: "v1beta2",
		Kind:    "Kyma",
	})

	err := s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)

	assert.NoError(s.t, err)
}

func (s *BrokerSuiteTest) AssertKymaResourceNotExists(opId string) {
	operation, err := s.db.Operations().GetOperationByID(opId)
	assert.NoError(s.t, err)

	obj := &unstructured.Unstructured{}
	obj.SetName(operation.RuntimeID)
	obj.SetNamespace("kyma-system")
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.kyma-project.io",
		Version: "v1beta2",
		Kind:    "Kyma",
	})

	err = s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)

	assert.Error(s.t, err)
}

func (s *BrokerSuiteTest) AssertKymaAnnotationExists(opId, annotationName string) {
	operation, err := s.db.Operations().GetOperationByID(opId)
	assert.NoError(s.t, err)
	obj := &unstructured.Unstructured{}
	obj.SetName(operation.RuntimeID)
	obj.SetNamespace("kyma-system")
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.kyma-project.io",
		Version: "v1beta2",
		Kind:    "Kyma",
	})

	err = s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)

	assert.Contains(s.t, obj.GetAnnotations(), annotationName)
}

func (s *BrokerSuiteTest) AssertKymaLabelsExist(opId string, expectedLabels map[string]string) {
	operation, err := s.db.Operations().GetOperationByID(opId)
	assert.NoError(s.t, err)
	obj := &unstructured.Unstructured{}
	obj.SetName(operation.RuntimeID)
	obj.SetNamespace("kyma-system")
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.kyma-project.io",
		Version: "v1beta2",
		Kind:    "Kyma",
	})

	err = s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)

	assert.Subset(s.t, obj.GetLabels(), expectedLabels)
}

func (s *BrokerSuiteTest) AssertKymaLabelNotExists(opId string, notExpectedLabel string) {
	operation, err := s.db.Operations().GetOperationByID(opId)
	assert.NoError(s.t, err)
	obj := &unstructured.Unstructured{}
	obj.SetName(operation.RuntimeID)
	obj.SetNamespace("kyma-system")
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.kyma-project.io",
		Version: "v1beta2",
		Kind:    "Kyma",
	})

	err = s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)

	assert.NotContains(s.t, obj.GetLabels(), notExpectedLabel)
}

func (s *BrokerSuiteTest) AssertSecretWithKubeconfigExists(opId string) {
	operation, err := s.db.Operations().GetOperationByID(opId)
	assert.NoError(s.t, err)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "kyma-system",
			Name:      fmt.Sprintf("kubeconfig-%s", operation.RuntimeID),
		},
		StringData: map[string]string{},
	}
	err = s.k8sKcp.Get(context.Background(), client.ObjectKeyFromObject(secret), secret)

	assert.NoError(s.t, err)

}

func (s *BrokerSuiteTest) fixServiceBindingAndInstances(t *testing.T) {
	createResource(t, serviceInstanceGvk, s.k8sSKR, kymaNamespace, instanceName)
	createResource(t, serviceBindingGvk, s.k8sSKR, kymaNamespace, bindingName)
}

func (s *BrokerSuiteTest) assertServiceBindingAndInstancesAreRemoved(t *testing.T) {
	assertResourcesAreRemoved(t, serviceInstanceGvk, s.k8sSKR)
	assertResourcesAreRemoved(t, serviceBindingGvk, s.k8sSKR)
}

func assertResourcesAreRemoved(t *testing.T, gvk schema.GroupVersionKind, k8sClient client.Client) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	err := k8sClient.List(context.TODO(), list)
	assert.NoError(t, err)
	assert.Zero(t, len(list.Items))
}

func createResource(t *testing.T, gvk schema.GroupVersionKind, k8sClient client.Client, namespace string, name string) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	object.SetNamespace(namespace)
	object.SetName(name)
	err := k8sClient.Create(context.TODO(), object)
	assert.NoError(t, err)
}

func mockBTPOperatorClusterID() {
	mock := func(string) (string, error) {
		return "cluster_id", nil
	}
	update.ConfigMapGetter = mock
	upgrade_kyma.ConfigMapGetter = mock
}

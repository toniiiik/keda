package scalers

/*
Copyright 2021 The KEDA Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	eventhub "github.com/Azure/azure-event-hubs-go/v3"
	"github.com/Azure/azure-storage-blob-go/azblob"
	az "github.com/Azure/go-autorest/autorest/azure"
	"github.com/go-logr/logr"
	v2 "k8s.io/api/autoscaling/v2"
	"k8s.io/metrics/pkg/apis/external_metrics"

	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/scalers/azure"
	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

const (
	defaultEventHubMessageThreshold    = 64
	eventHubMetricType                 = "External"
	thresholdMetricName                = "unprocessedEventThreshold"
	activationThresholdMetricName      = "activationUnprocessedEventThreshold"
	defaultEventHubConsumerGroup       = "$Default"
	defaultBlobContainer               = ""
	defaultCheckpointStrategy          = ""
	defaultStalePartitionInfoThreshold = 10000
)

type azureEventHubScaler struct {
	metricType v2.MetricTargetType
	metadata   *eventHubMetadata
	client     *eventhub.Hub
	httpClient *http.Client
	logger     logr.Logger
}

type eventHubMetadata struct {
	eventHubInfo                azure.EventHubInfo
	threshold                   int64
	activationThreshold         int64
	stalePartitionInfoThreshold int64
	triggerIndex                int
}

// NewAzureEventHubScaler creates a new scaler for eventHub
func NewAzureEventHubScaler(ctx context.Context, config *ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %w", err)
	}

	logger := InitializeLogger(config, "azure_eventhub_scaler")

	parsedMetadata, err := parseAzureEventHubMetadata(logger, config)
	if err != nil {
		return nil, fmt.Errorf("unable to get eventhub metadata: %w", err)
	}

	hub, err := azure.GetEventHubClient(ctx, parsedMetadata.eventHubInfo)
	if err != nil {
		return nil, fmt.Errorf("unable to get eventhub client: %w", err)
	}

	return &azureEventHubScaler{
		metricType: metricType,
		metadata:   parsedMetadata,
		client:     hub,
		httpClient: kedautil.CreateHTTPClient(config.GlobalHTTPTimeout, false),
		logger:     logger,
	}, nil
}

// parseAzureEventHubMetadata parses metadata
func parseAzureEventHubMetadata(logger logr.Logger, config *ScalerConfig) (*eventHubMetadata, error) {
	meta := eventHubMetadata{
		eventHubInfo: azure.EventHubInfo{},
	}

	err := parseCommonAzureEventHubMetadata(config, &meta)
	if err != nil {
		return nil, err
	}

	err = parseAzureEventHubAuthenticationMetadata(logger, config, &meta)
	if err != nil {
		return nil, err
	}

	return &meta, nil
}

func parseCommonAzureEventHubMetadata(config *ScalerConfig, meta *eventHubMetadata) error {
	meta.threshold = defaultEventHubMessageThreshold

	if val, ok := config.TriggerMetadata[thresholdMetricName]; ok {
		threshold, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("error parsing azure eventhub metadata %s: %w", thresholdMetricName, err)
		}

		meta.threshold = threshold
	}

	meta.activationThreshold = 0
	if val, ok := config.TriggerMetadata[activationThresholdMetricName]; ok {
		activationThreshold, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("error parsing azure eventhub metadata %s: %w", activationThresholdMetricName, err)
		}

		meta.activationThreshold = activationThreshold
	}

	if config.AuthParams["storageConnection"] != "" {
		meta.eventHubInfo.StorageConnection = config.AuthParams["storageConnection"]
	} else if config.TriggerMetadata["storageConnectionFromEnv"] != "" {
		meta.eventHubInfo.StorageConnection = config.ResolvedEnv[config.TriggerMetadata["storageConnectionFromEnv"]]
	}

	meta.eventHubInfo.EventHubConsumerGroup = defaultEventHubConsumerGroup
	if val, ok := config.TriggerMetadata["consumerGroup"]; ok {
		meta.eventHubInfo.EventHubConsumerGroup = val
	}

	meta.eventHubInfo.CheckpointStrategy = defaultCheckpointStrategy
	if val, ok := config.TriggerMetadata["checkpointStrategy"]; ok {
		meta.eventHubInfo.CheckpointStrategy = val
	}

	meta.eventHubInfo.BlobContainer = defaultBlobContainer
	if val, ok := config.TriggerMetadata["blobContainer"]; ok {
		meta.eventHubInfo.BlobContainer = val
	}

	meta.eventHubInfo.EventHubResourceURL = azure.DefaultEventhubResourceURL
	if val, ok := config.TriggerMetadata["cloud"]; ok {
		if strings.EqualFold(val, azure.PrivateCloud) {
			if resourceURL, ok := config.TriggerMetadata["eventHubResourceURL"]; ok {
				meta.eventHubInfo.EventHubResourceURL = resourceURL
			} else {
				return fmt.Errorf("eventHubResourceURL must be provided for %s cloud type", azure.PrivateCloud)
			}
		}
	}

	serviceBusEndpointSuffixProvider := func(env az.Environment) (string, error) {
		return env.ServiceBusEndpointSuffix, nil
	}
	serviceBusEndpointSuffix, err := azure.ParseEnvironmentProperty(config.TriggerMetadata, azure.DefaultEndpointSuffixKey, serviceBusEndpointSuffixProvider)
	if err != nil {
		return err
	}
	meta.eventHubInfo.ServiceBusEndpointSuffix = serviceBusEndpointSuffix

	activeDirectoryEndpoint, err := azure.ParseActiveDirectoryEndpoint(config.TriggerMetadata)
	if err != nil {
		return err
	}
	meta.eventHubInfo.ActiveDirectoryEndpoint = activeDirectoryEndpoint

	meta.stalePartitionInfoThreshold = defaultStalePartitionInfoThreshold
	if val, ok := config.TriggerMetadata["stalePartitionInfoThreshold"]; ok {
		stalePartitionInfoThreshold, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("error parsing azure eventhub metadata stalePartitionInfoThreshold: %w", err)
		}
		meta.stalePartitionInfoThreshold = stalePartitionInfoThreshold
	}

	meta.triggerIndex = config.TriggerIndex

	return nil
}

func parseAzureEventHubAuthenticationMetadata(logger logr.Logger, config *ScalerConfig, meta *eventHubMetadata) error {
	meta.eventHubInfo.PodIdentity = config.PodIdentity

	switch config.PodIdentity.Provider {
	case "", v1alpha1.PodIdentityProviderNone:
		if len(meta.eventHubInfo.StorageConnection) == 0 {
			return fmt.Errorf("no storage connection string given")
		}

		connection := ""
		if config.AuthParams["connection"] != "" {
			connection = config.AuthParams["connection"]
		} else if config.TriggerMetadata["connectionFromEnv"] != "" {
			connection = config.ResolvedEnv[config.TriggerMetadata["connectionFromEnv"]]
		}

		if len(connection) == 0 {
			return fmt.Errorf("no event hub connection string given")
		}

		if !strings.Contains(connection, "EntityPath") {
			eventHubName := ""
			if config.TriggerMetadata["eventHubName"] != "" {
				eventHubName = config.TriggerMetadata["eventHubName"]
			} else if config.TriggerMetadata["eventHubNameFromEnv"] != "" {
				eventHubName = config.ResolvedEnv[config.TriggerMetadata["eventHubNameFromEnv"]]
			}

			if eventHubName == "" {
				return fmt.Errorf("connection string does not contain event hub name, and parameter eventHubName not provided")
			}

			connection = fmt.Sprintf("%s;EntityPath=%s", connection, eventHubName)
		}

		meta.eventHubInfo.EventHubConnection = connection
	case v1alpha1.PodIdentityProviderAzure, v1alpha1.PodIdentityProviderAzureWorkload:
		meta.eventHubInfo.StorageAccountName = ""
		if val, ok := config.TriggerMetadata["storageAccountName"]; ok {
			meta.eventHubInfo.StorageAccountName = val
		} else {
			logger.Info("no 'storageAccountName' provided to enable identity based authentication to Blob Storage. Attempting to use connection string instead")
		}

		if len(meta.eventHubInfo.StorageAccountName) != 0 {
			storageEndpointSuffixProvider := func(env az.Environment) (string, error) {
				return env.StorageEndpointSuffix, nil
			}
			storageEndpointSuffix, err := azure.ParseEnvironmentProperty(config.TriggerMetadata, azure.DefaultStorageSuffixKey, storageEndpointSuffixProvider)
			if err != nil {
				return err
			}
			meta.eventHubInfo.BlobStorageEndpoint = "blob." + storageEndpointSuffix
		}

		if len(meta.eventHubInfo.StorageConnection) == 0 && len(meta.eventHubInfo.StorageAccountName) == 0 {
			return fmt.Errorf("no storage connection string or storage account name for pod identity based authentication given")
		}

		if config.TriggerMetadata["eventHubNamespace"] != "" {
			meta.eventHubInfo.Namespace = config.TriggerMetadata["eventHubNamespace"]
		} else if config.TriggerMetadata["eventHubNamespaceFromEnv"] != "" {
			meta.eventHubInfo.Namespace = config.ResolvedEnv[config.TriggerMetadata["eventHubNamespaceFromEnv"]]
		}

		if len(meta.eventHubInfo.Namespace) == 0 {
			return fmt.Errorf("no event hub namespace string given")
		}

		if config.TriggerMetadata["eventHubName"] != "" {
			meta.eventHubInfo.EventHubName = config.TriggerMetadata["eventHubName"]
		} else if config.TriggerMetadata["eventHubNameFromEnv"] != "" {
			meta.eventHubInfo.EventHubName = config.ResolvedEnv[config.TriggerMetadata["eventHubNameFromEnv"]]
		}

		if len(meta.eventHubInfo.EventHubName) == 0 {
			return fmt.Errorf("no event hub name string given")
		}
	}

	return nil
}

// GetUnprocessedEventCountInPartition gets number of unprocessed events in a given partition
func (s *azureEventHubScaler) GetUnprocessedEventCountInPartition(ctx context.Context, partitionInfo *eventhub.HubPartitionRuntimeInformation) (newEventCount int64, checkpoint azure.Checkpoint, err error) {
	// if partitionInfo.LastEnqueuedOffset = -1, that means event hub partition is empty
	if partitionInfo == nil || partitionInfo.LastEnqueuedOffset == "-1" {
		return 0, azure.Checkpoint{}, nil
	}

	checkpoint, err = azure.GetCheckpointFromBlobStorage(ctx, s.httpClient, s.metadata.eventHubInfo, partitionInfo.PartitionID)
	if err != nil {
		// if blob not found return the total partition event count
		err = errors.Unwrap(err)
		if stErr, ok := err.(azblob.StorageError); ok {
			if stErr.ServiceCode() == azblob.ServiceCodeBlobNotFound || stErr.ServiceCode() == azblob.ServiceCodeContainerNotFound {
				s.logger.V(1).Error(err, fmt.Sprintf("Blob container : %s not found to use checkpoint strategy, getting unprocessed event count without checkpoint", s.metadata.eventHubInfo.BlobContainer))
				return GetUnprocessedEventCountWithoutCheckpoint(partitionInfo), azure.Checkpoint{}, nil
			}
		}
		return -1, azure.Checkpoint{}, fmt.Errorf("unable to get checkpoint from storage: %w", err)
	}

	unprocessedEventCountInPartition := calculateUnprocessedEvents(partitionInfo, checkpoint, s.metadata.stalePartitionInfoThreshold)

	return unprocessedEventCountInPartition, checkpoint, nil
}

func calculateUnprocessedEvents(partitionInfo *eventhub.HubPartitionRuntimeInformation, checkpoint azure.Checkpoint, stalePartitionInfoThreshold int64) int64 {
	unprocessedEventCount := int64(0)

	// If checkpoint.Offset is empty that means no messages has been processed from an event hub partition
	// And since partitionInfo.LastSequenceNumber = 0 for the very first message hence
	// total unprocessed message will be partitionInfo.LastSequenceNumber + 1
	if checkpoint.Offset == "" {
		unprocessedEventCount = partitionInfo.LastSequenceNumber + 1
		return unprocessedEventCount
	}

	if partitionInfo.LastSequenceNumber >= checkpoint.SequenceNumber {
		unprocessedEventCount = partitionInfo.LastSequenceNumber - checkpoint.SequenceNumber
	} else {
		// Partition is a circular buffer, so it is possible that
		// partitionInfo.LastSequenceNumber < blob checkpoint's SequenceNumber

		// Checkpointing may or may not be always behind partition's LastSequenceNumber.
		// The partition information read could be stale compared to checkpoint,
		// especially when load is very small and checkpointing is happening often.
		// This also results in partitionInfo.LastSequenceNumber < blob checkpoint's SequenceNumber
		// e.g., (9223372036854775807 - 15) + 10 = 9223372036854775802

		// Calculate the unprocessed events
		unprocessedEventCount = (math.MaxInt64 - checkpoint.SequenceNumber) + partitionInfo.LastSequenceNumber
	}

	// If the result is greater than the buffer size - stale partition threshold
	// we assume the partition info is stale.
	if unprocessedEventCount > (math.MaxInt64 - stalePartitionInfoThreshold) {
		return 0
	}

	return unprocessedEventCount
}

// GetUnprocessedEventCountWithoutCheckpoint returns the number of messages on the without a checkoutpoint info
func GetUnprocessedEventCountWithoutCheckpoint(partitionInfo *eventhub.HubPartitionRuntimeInformation) int64 {
	// if both values are 0 then there is exactly one message inside the hub. First message after init
	if (partitionInfo.BeginningSequenceNumber == 0 && partitionInfo.LastSequenceNumber == 0) || (partitionInfo.BeginningSequenceNumber != partitionInfo.LastSequenceNumber) {
		return (partitionInfo.LastSequenceNumber - partitionInfo.BeginningSequenceNumber) + 1
	}

	return 0
}

// GetMetricSpecForScaling returns metric spec
func (s *azureEventHubScaler) GetMetricSpecForScaling(context.Context) []v2.MetricSpec {
	externalMetric := &v2.ExternalMetricSource{
		Metric: v2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.triggerIndex, kedautil.NormalizeString(fmt.Sprintf("azure-eventhub-%s", s.metadata.eventHubInfo.EventHubConsumerGroup))),
		},
		Target: GetMetricTarget(s.metricType, s.metadata.threshold),
	}
	metricSpec := v2.MetricSpec{External: externalMetric, Type: eventHubMetricType}
	return []v2.MetricSpec{metricSpec}
}

func getTotalLagRelatedToPartitionAmount(unprocessedEventsCount int64, partitionCount int64, threshold int64) int64 {
	if (unprocessedEventsCount / threshold) > partitionCount {
		return partitionCount * threshold
	}

	return unprocessedEventsCount
}

// Close closes Azure Event Hub Scaler
func (s *azureEventHubScaler) Close(ctx context.Context) error {
	if s.client != nil {
		err := s.client.Close(ctx)
		if err != nil {
			s.logger.Error(err, "error closing azure event hub client")
			return err
		}
	}

	return nil
}

// GetMetricsAndActivity returns value for a supported metric and an error if there is a problem getting the metric
func (s *azureEventHubScaler) GetMetricsAndActivity(ctx context.Context, metricName string) ([]external_metrics.ExternalMetricValue, bool, error) {
	totalUnprocessedEventCount := int64(0)
	runtimeInfo, err := s.client.GetRuntimeInformation(ctx)
	if err != nil {
		return []external_metrics.ExternalMetricValue{}, false, fmt.Errorf("unable to get runtimeInfo for metrics: %w", err)
	}

	partitionIDs := runtimeInfo.PartitionIDs

	for i := 0; i < len(partitionIDs); i++ {
		partitionID := partitionIDs[i]
		partitionRuntimeInfo, err := s.client.GetPartitionInformation(ctx, partitionID)
		if err != nil {
			return []external_metrics.ExternalMetricValue{}, false, fmt.Errorf("unable to get partitionRuntimeInfo for metrics: %w", err)
		}

		unprocessedEventCount := int64(0)

		unprocessedEventCount, checkpoint, err := s.GetUnprocessedEventCountInPartition(ctx, partitionRuntimeInfo)
		if err != nil {
			return []external_metrics.ExternalMetricValue{}, false, fmt.Errorf("unable to get unprocessedEventCount for metrics: %w", err)
		}

		totalUnprocessedEventCount += unprocessedEventCount

		s.logger.V(1).Info(fmt.Sprintf("Partition ID: %s, Last SequenceNumber: %d, Checkpoint SequenceNumber: %d, Total new events in partition: %d",
			partitionRuntimeInfo.PartitionID, partitionRuntimeInfo.LastSequenceNumber, checkpoint.SequenceNumber, unprocessedEventCount))
	}

	// set count to max if the sum is negative (Int64 overflow) to prevent negative metric values
	// e.g., 9223372036854775797 (Partition 1) + 20 (Partition 2) = -9223372036854775799
	if totalUnprocessedEventCount < 0 {
		totalUnprocessedEventCount = math.MaxInt64
	}

	// don't scale out beyond the number of partitions
	lagRelatedToPartitionCount := getTotalLagRelatedToPartitionAmount(totalUnprocessedEventCount, int64(len(partitionIDs)), s.metadata.threshold)

	s.logger.V(1).Info(fmt.Sprintf("Unprocessed events in event hub total: %d, scaling for a lag of %d related to %d partitions", totalUnprocessedEventCount, lagRelatedToPartitionCount, len(partitionIDs)))

	metric := GenerateMetricInMili(metricName, float64(lagRelatedToPartitionCount))

	return []external_metrics.ExternalMetricValue{metric}, totalUnprocessedEventCount > s.metadata.activationThreshold, nil
}

package v2

import (
	"context"
	"fmt"
	"time"

	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	backoff "github.com/cenkalti/backoff/v4"
	"github.com/ethpandaops/xatu/pkg/cannon/ethereum"
	"github.com/ethpandaops/xatu/pkg/cannon/iterator"
	xatuethv1 "github.com/ethpandaops/xatu/pkg/proto/eth/v1"
	xatuethv2 "github.com/ethpandaops/xatu/pkg/proto/eth/v2"
	"github.com/ethpandaops/xatu/pkg/proto/xatu"
	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	BLSToExecutionChangeDeriverName = xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_BLS_TO_EXECUTION_CHANGE
)

type BLSToExecutionChangeDeriverConfig struct {
	Enabled     bool    `yaml:"enabled" default:"true"`
	HeadSlotLag *uint64 `yaml:"headSlotLag" default:"1"`
}

type BLSToExecutionChangeDeriver struct {
	log                 logrus.FieldLogger
	cfg                 *BLSToExecutionChangeDeriverConfig
	iterator            *iterator.SlotIterator
	onEventCallbacks    []func(ctx context.Context, event *xatu.DecoratedEvent) error
	onLocationCallbacks []func(ctx context.Context, loc uint64) error
	beacon              *ethereum.BeaconNode
	clientMeta          *xatu.ClientMeta
}

func NewBLSToExecutionChangeDeriver(log logrus.FieldLogger, config *BLSToExecutionChangeDeriverConfig, iter *iterator.SlotIterator, beacon *ethereum.BeaconNode, clientMeta *xatu.ClientMeta) *BLSToExecutionChangeDeriver {
	return &BLSToExecutionChangeDeriver{
		log:        log.WithField("module", "cannon/event/beacon/eth/v2/bls_to_execution_change"),
		cfg:        config,
		iterator:   iter,
		beacon:     beacon,
		clientMeta: clientMeta,
	}
}

func (b *BLSToExecutionChangeDeriver) CannonType() xatu.CannonType {
	return BLSToExecutionChangeDeriverName
}

func (b *BLSToExecutionChangeDeriver) Name() string {
	return BLSToExecutionChangeDeriverName.String()
}

func (b *BLSToExecutionChangeDeriver) OnEventDerived(ctx context.Context, fn func(ctx context.Context, event *xatu.DecoratedEvent) error) {
	b.onEventCallbacks = append(b.onEventCallbacks, fn)
}

func (b *BLSToExecutionChangeDeriver) OnLocationUpdated(ctx context.Context, fn func(ctx context.Context, location uint64) error) {
	b.onLocationCallbacks = append(b.onLocationCallbacks, fn)
}

func (b *BLSToExecutionChangeDeriver) Start(ctx context.Context) error {
	if !b.cfg.Enabled {
		b.log.Info("BLS to execution change deriver disabled")

		return nil
	}

	b.log.Info("BLS to execution change deriver enabled")

	// Start our main loop
	go b.run(ctx)

	return nil
}

func (b *BLSToExecutionChangeDeriver) Stop(ctx context.Context) error {
	return nil
}

func (b *BLSToExecutionChangeDeriver) run(ctx context.Context) {
	bo := backoff.NewExponentialBackOff()
	bo.MaxInterval = 1 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		default:
			operation := func() error {
				time.Sleep(100 * time.Millisecond)

				if err := b.beacon.Synced(ctx); err != nil {
					return err
				}

				// Get the next slot
				location, err := b.iterator.Next(ctx)
				if err != nil {
					return err
				}

				for _, fn := range b.onLocationCallbacks {
					if errr := fn(ctx, location.GetEthV2BeaconBlockBlsToExecutionChange().GetSlot()); errr != nil {
						b.log.WithError(errr).Error("Failed to send location")
					}
				}

				// Process the slot
				events, err := b.processSlot(ctx, phase0.Slot(location.GetEthV2BeaconBlockBlsToExecutionChange().GetSlot()))
				if err != nil {
					b.log.WithError(err).Error("Failed to process slot")

					return err
				}

				// Send the events
				for _, event := range events {
					for _, fn := range b.onEventCallbacks {
						if err := fn(ctx, event); err != nil {
							b.log.WithError(err).Error("Failed to send event")
						}
					}
				}

				// Update our location
				if err := b.iterator.UpdateLocation(ctx, location); err != nil {
					return err
				}

				bo.Reset()

				return nil
			}

			if err := backoff.RetryNotify(operation, bo, func(err error, timer time.Duration) {
				b.log.WithError(err).WithField("next_attempt", timer).Warn("Failed to process")
			}); err != nil {
				b.log.WithError(err).Warn("Failed to process")
			}
		}
	}
}

func (b *BLSToExecutionChangeDeriver) processSlot(ctx context.Context, slot phase0.Slot) ([]*xatu.DecoratedEvent, error) {
	// Get the block
	block, err := b.beacon.GetBeaconBlock(ctx, xatuethv1.SlotAsString(slot))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get beacon block for slot %d", slot)
	}

	if block == nil {
		return []*xatu.DecoratedEvent{}, nil
	}

	blockIdentifier, err := GetBlockIdentifier(block, b.beacon.Metadata().Wallclock())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get block identifier for slot %d", slot)
	}

	events := []*xatu.DecoratedEvent{}

	changes, err := b.getBLSToExecutionChanges(ctx, block)
	if err != nil {
		return nil, err
	}

	for _, change := range changes {
		event, err := b.createEvent(ctx, change, blockIdentifier)
		if err != nil {
			b.log.WithError(err).Error("Failed to create event")

			return nil, errors.Wrapf(err, "failed to create event for BLS to execution change %s", change.String())
		}

		events = append(events, event)
	}

	return events, nil
}

func (b *BLSToExecutionChangeDeriver) getBLSToExecutionChanges(ctx context.Context, block *spec.VersionedSignedBeaconBlock) ([]*xatuethv2.SignedBLSToExecutionChangeV2, error) {
	changes := []*xatuethv2.SignedBLSToExecutionChangeV2{}

	switch block.Version {
	case spec.DataVersionPhase0:
		return changes, nil
	case spec.DataVersionAltair:
		return changes, nil
	case spec.DataVersionBellatrix:
		return changes, nil
	case spec.DataVersionCapella:
		for _, change := range block.Capella.Message.Body.BLSToExecutionChanges {
			changes = append(changes, &xatuethv2.SignedBLSToExecutionChangeV2{
				Message: &xatuethv2.BLSToExecutionChangeV2{
					ValidatorIndex:     wrapperspb.UInt64(uint64(change.Message.ValidatorIndex)),
					FromBlsPubkey:      change.Message.FromBLSPubkey.String(),
					ToExecutionAddress: change.Message.ToExecutionAddress.String(),
				},
				Signature: change.Signature.String(),
			})
		}
	default:
		return nil, fmt.Errorf("unsupported block version: %s", block.Version.String())
	}

	return changes, nil
}

func (b *BLSToExecutionChangeDeriver) createEvent(ctx context.Context, change *xatuethv2.SignedBLSToExecutionChangeV2, identifier *xatu.BlockIdentifier) (*xatu.DecoratedEvent, error) {
	// Make a clone of the metadata
	metadata, ok := proto.Clone(b.clientMeta).(*xatu.ClientMeta)
	if !ok {
		return nil, errors.New("failed to clone client metadata")
	}

	decoratedEvent := &xatu.DecoratedEvent{
		Event: &xatu.Event{
			Name:     xatu.Event_BEACON_API_ETH_V2_BEACON_BLOCK_BLS_TO_EXECUTION_CHANGE,
			DateTime: timestamppb.New(time.Now()),
			Id:       uuid.New().String(),
		},
		Meta: &xatu.Meta{
			Client: metadata,
		},
		Data: &xatu.DecoratedEvent_EthV2BeaconBlockBlsToExecutionChange{
			EthV2BeaconBlockBlsToExecutionChange: change,
		},
	}

	decoratedEvent.Meta.Client.AdditionalData = &xatu.ClientMeta_EthV2BeaconBlockBlsToExecutionChange{
		EthV2BeaconBlockBlsToExecutionChange: &xatu.ClientMeta_AdditionalEthV2BeaconBlockBLSToExecutionChangeData{
			Block: identifier,
		},
	}

	return decoratedEvent, nil
}

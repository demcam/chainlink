package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/pelletier/go-toml"
	"github.com/pkg/errors"
	clipkg "github.com/urfave/cli"

	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/chainlink"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/keystore"
	"github.com/smartcontractkit/chainlink/core/services/keystore/chaintype"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/ocr2key"
	"github.com/smartcontractkit/chainlink/core/services/pg"
	"github.com/smartcontractkit/chainlink/core/static"
)

type SetupOCR2VRFNodePayload struct {
	OnChainPublicKey  string
	OffChainPublicKey string
	ConfigPublicKey   string
	PeerID            string
	Transmitter       string
	DkgEncrypt        string
	DkgSign           string
}

type dkgTemplateArgs struct {
	contractID              string
	ocrKeyBundleID          string
	p2pv2BootstrapperPeerID string
	p2pv2BootstrapperPort   string
	transmitterID           string
	chainID                 int64
	encryptionPublicKey     string
	keyID                   string
	signingPublicKey        string
}

type ocr2vrfTemplateArgs struct {
	dkgTemplateArgs
	vrfContractAddress string
	linkEthFeedAddress string
	confirmationDelays string
	lookbackBlocks     int64
}

const dkgTemplate = `
# DKGSpec
type                 = "offchainreporting2"
schemaVersion        = 1
name                 = "ocr2"
maxTaskDuration      = "30s"
contractID           = "%s"
ocrKeyBundleID       = "%s"
p2pv2Bootstrappers   = ["%s@127.0.0.1:%s"]
relay                = "evm"
pluginType           = "dkg"
transmitterID        = "%s"

[relayConfig]
chainID              = %d

[pluginConfig]
EncryptionPublicKey  = "%s"
KeyID                = "%s"
SigningPublicKey     = "%s"
`

const ocr2vrfTemplate = `
type                 = "offchainreporting2"
schemaVersion        = 1
name                 = "ocr2"
maxTaskDuration      = "30s"
contractID           = "%s"
ocrKeyBundleID       = "%s"
p2pv2Bootstrappers   = ["%s@127.0.0.1:%s"]
relay                = "evm"
pluginType           = "ocr2vrf"
transmitterID        = "%s"

[relayConfig]
chainID              = %d

[pluginConfig]
dkgEncryptionPublicKey = "%s"
dkgSigningPublicKey    = "%s"
dkgKeyID               = "%s"
dkgContractAddress     = "%s"

linkEthFeedAddress     = "%s"
confirmationDelays     = %s # This is an array
lookbackBlocks         = %d # This is an integer
`

const bootstrapTemplate = `
type                               = "bootstrap"
schemaVersion                      = 1
name                               = ""
id                                 = "1"
contractID                         = "%s"
relay                              = "evm"

[relayConfig]
chainID                            = %d
`

func (cli *Client) ConfigureOCR2VRFNode(c *clipkg.Context) (*SetupOCR2VRFNodePayload, error) {
	lggr := cli.Logger.Named("ConfigureOCR2VRFNode")
	err := cli.Config.Validate()
	if err != nil {
		return nil, cli.errorOut(errors.Wrap(err, "config validation failed"))
	}
	lggr.Infow(
		fmt.Sprintf("Configuring Chainlink Node for job type %s %s at commit %s", c.String("job-type"), static.Version, static.Sha),
		"Version", static.Version, "SHA", static.Sha)

	ldb := pg.NewLockedDB(cli.Config, lggr)
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err = ldb.Open(rootCtx); err != nil {
		return nil, cli.errorOut(errors.Wrap(err, "opening db"))
	}
	defer lggr.ErrorIfClosing(ldb, "db")

	app, err := cli.AppFactory.NewApplication(cli.Config, ldb.DB())
	if err != nil {
		return nil, cli.errorOut(errors.Wrap(err, "fatal error instantiating application"))
	}

	// Initialize keystore and generate keys.
	keyStore := app.GetKeyStore()
	err = setupKeystore(cli, c, app, keyStore)
	if err != nil {
		return nil, cli.errorOut(err)
	}

	// Get all configuration parameters.
	keyID := c.String("keyID")
	dkgEncrypt, _ := app.GetKeyStore().DKGEncrypt().GetAll()
	dkgSign, _ := app.GetKeyStore().DKGSign().GetAll()
	dkgEncryptKey := dkgEncrypt[0].PublicKeyString()
	dkgSignKey := dkgSign[0].PublicKeyString()
	p2p, _ := app.GetKeyStore().P2P().GetAll()
	ocr2List, _ := app.GetKeyStore().OCR2().GetAll()
	ethKeys, _ := app.GetKeyStore().Eth().GetAll()
	transmitterID := ethKeys[0].Address.String()
	peerID := p2p[0].PeerID().Raw()
	if c.Bool("isBootstrapper") == false {
		peerID = c.String("bootstrapperPeerID")
	}

	// Find the EVM OCR2 bundle.
	var ocr2 ocr2key.KeyBundle
	for _, ocr2Item := range ocr2List {
		if ocr2Item.ChainType() == chaintype.EVM {
			ocr2 = ocr2Item
		}
	}
	if ocr2 == nil {
		return nil, cli.errorOut(errors.Wrap(job.ErrNoSuchKeyBundle, "evm OCR2 key bundle not found"))
	}
	offChainPublicKey := ocr2.OffchainPublicKey()
	configPublicKey := ocr2.ConfigEncryptionPublicKey()

	if c.Bool("isBootstrapper") {
		// Set up bootstrapper job if bootstrapper.
		err = createBootstrapperJob(lggr, c, app)
	} else if c.String("job-type") == "DKG" {
		// Set up DKG job.
		err = createDKGJob(lggr, app, dkgTemplateArgs{
			contractID:              c.String("contractID"),
			ocrKeyBundleID:          ocr2.ID(),
			p2pv2BootstrapperPeerID: peerID,
			p2pv2BootstrapperPort:   c.String("bootstrapPort"),
			transmitterID:           transmitterID,
			chainID:                 c.Int64("chainID"),
			encryptionPublicKey:     dkgEncryptKey,
			keyID:                   keyID,
			signingPublicKey:        dkgSignKey,
		})
	} else if c.String("job-type") == "OCR2VRF" {
		// Set up OCR2VRF job.
		err = createOCR2VRFJob(lggr, app, ocr2vrfTemplateArgs{
			dkgTemplateArgs: dkgTemplateArgs{
				contractID:              c.String("dkg-address"),
				ocrKeyBundleID:          ocr2.ID(),
				p2pv2BootstrapperPeerID: peerID,
				p2pv2BootstrapperPort:   c.String("bootstrapPort"),
				transmitterID:           transmitterID,
				chainID:                 c.Int64("chainID"),
				encryptionPublicKey:     dkgEncryptKey,
				keyID:                   keyID,
				signingPublicKey:        dkgSignKey,
			},
			vrfContractAddress: c.String("vrf-address"),
			linkEthFeedAddress: c.String("link-eth-feed-address"),
			lookbackBlocks:     c.Int64("lookback-blocks"),
			confirmationDelays: c.String("confirmation-delays"),
		})
	} else {
		err = fmt.Errorf("unknown job type: %s", c.String("job-type"))
	}
	if err != nil {
		return nil, err
	}

	return &SetupOCR2VRFNodePayload{
		OnChainPublicKey:  ocr2.OnChainPublicKey(),
		OffChainPublicKey: hex.EncodeToString(offChainPublicKey[:]),
		ConfigPublicKey:   hex.EncodeToString(configPublicKey[:]),
		PeerID:            p2p[0].PeerID().Raw(),
		Transmitter:       transmitterID,
		DkgEncrypt:        dkgEncryptKey,
		DkgSign:           dkgSignKey,
	}, nil
}

func setupKeystore(cli *Client, c *clipkg.Context, app chainlink.Application, keyStore keystore.Master) error {
	err := cli.KeyStoreAuthenticator.authenticate(c, keyStore)
	if err != nil {
		return errors.Wrap(err, "error authenticating keystore")
	}

	evmChainSet := app.GetChains().EVM
	if cli.Config.EVMEnabled() {
		if err != nil {
			return errors.Wrap(err, "error migrating keystore")
		}

		for _, ch := range evmChainSet.Chains() {
			err = keyStore.Eth().EnsureKeys(ch.ID())
			if err != nil {
				return errors.Wrap(err, "failed to ensure keystore keys")
			}
		}
	}

	err = keyStore.OCR2().EnsureKeys()
	if err != nil {
		return errors.Wrap(err, "failed to ensure ocr key")
	}

	err = keyStore.DKGSign().EnsureKey()
	if err != nil {
		return errors.Wrap(err, "failed to ensure ocr key")
	}

	err = keyStore.DKGEncrypt().EnsureKey()
	if err != nil {
		return errors.Wrap(err, "failed to ensure ocr key")
	}

	err = keyStore.P2P().EnsureKey()
	if err != nil {
		return errors.Wrap(err, "failed to ensure p2p key")
	}

	return nil
}

func createBootstrapperJob(lggr logger.Logger, c *clipkg.Context, app chainlink.Application) error {
	sp := fmt.Sprintf(bootstrapTemplate,
		c.String("contractID"),
		c.Int64("chainID"),
	)
	var jb job.Job
	err := toml.Unmarshal([]byte(sp), &jb)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	var os job.BootstrapSpec
	err = toml.Unmarshal([]byte(sp), &os)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	jb.BootstrapSpec = &os

	err = app.AddJobV2(context.Background(), &jb)
	if err != nil {
		return errors.Wrap(err, "failed to add job")
	}
	lggr.Info("bootstrap spec:", sp)

	// Give a cooldown
	time.Sleep(time.Second)

	return nil
}

func createDKGJob(lggr logger.Logger, app chainlink.Application, args dkgTemplateArgs) error {
	sp := fmt.Sprintf(dkgTemplate,
		args.contractID,
		args.ocrKeyBundleID,
		args.p2pv2BootstrapperPeerID,
		args.p2pv2BootstrapperPort,
		args.transmitterID,
		args.chainID,
		args.encryptionPublicKey,
		args.keyID,
		args.signingPublicKey,
	)

	var jb job.Job
	err := toml.Unmarshal([]byte(sp), &jb)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	var os job.OCR2OracleSpec
	err = toml.Unmarshal([]byte(sp), &os)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	jb.OCR2OracleSpec = &os

	err = app.AddJobV2(context.Background(), &jb)
	if err != nil {
		return errors.Wrap(err, "failed to add job")
	}
	lggr.Info("dkg spec:", sp)

	return nil
}

func createOCR2VRFJob(lggr logger.Logger, app chainlink.Application, args ocr2vrfTemplateArgs) error {
	sp := fmt.Sprintf(ocr2vrfTemplate,
		args.vrfContractAddress,
		args.ocrKeyBundleID,
		args.p2pv2BootstrapperPeerID,
		args.p2pv2BootstrapperPort,
		args.transmitterID,
		args.chainID,
		args.encryptionPublicKey,
		args.signingPublicKey,
		args.keyID,
		args.contractID,
		args.linkEthFeedAddress,
		fmt.Sprintf("[%s]", args.confirmationDelays), // conf delays should be comma separated
		args.lookbackBlocks,
	)

	var jb job.Job
	err := toml.Unmarshal([]byte(sp), &jb)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	var os job.OCR2OracleSpec
	err = toml.Unmarshal([]byte(sp), &os)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	jb.OCR2OracleSpec = &os

	err = app.AddJobV2(context.Background(), &jb)
	if err != nil {
		return errors.Wrap(err, "failed to add job")
	}
	lggr.Info("ocr2vrf spec:", sp)

	return nil
}

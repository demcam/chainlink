package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/smartcontractkit/libocr/offchainreporting2/confighelper"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/ocr2vrf/altbn_128"
	"github.com/smartcontractkit/ocr2vrf/dkg"
	"github.com/smartcontractkit/ocr2vrf/ocr2vrf"
	ocr2vrftypes "github.com/smartcontractkit/ocr2vrf/types"
	"github.com/urfave/cli"
	"go.dedis.ch/kyber/v3"
	"go.dedis.ch/kyber/v3/group/edwards25519"

	"github.com/smartcontractkit/chainlink/core/cmd"
	"github.com/smartcontractkit/chainlink/core/config"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/ocr2vrf/generated/vrf_beacon_consumer"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/ocr2vrf/generated/vrf_beacon_coordinator"
	"github.com/smartcontractkit/chainlink/core/logger"

	dkgContract "github.com/smartcontractkit/chainlink/core/internal/gethwrappers/ocr2vrf/generated/dkg"

	helpers "github.com/smartcontractkit/chainlink/core/scripts/common"
)

func deployDKG(e helpers.Environment) common.Address {
	_, tx, _, err := dkgContract.DeployDKG(e.Owner, e.Ec)
	helpers.PanicErr(err)
	return helpers.ConfirmContractDeployed(context.Background(), e.Ec, tx, e.ChainID)
}

func deployVRFBeaconCoordinator(e helpers.Environment, linkAddress, dkgAddress, keyID string, beaconPeriodBlocks *big.Int) common.Address {
	keyIDBytes := decodeHexTo32ByteArray(keyID)
	_, tx, _, err := vrf_beacon_coordinator.DeployVRFBeaconCoordinator(e.Owner, e.Ec, common.HexToAddress(linkAddress), beaconPeriodBlocks, common.HexToAddress(dkgAddress), keyIDBytes)
	helpers.PanicErr(err)
	return helpers.ConfirmContractDeployed(context.Background(), e.Ec, tx, e.ChainID)
}

func deployVRFBeaconCoordinatorConsumer(e helpers.Environment, coordinatorAddress string, shouldFail bool, beaconPeriodBlocks *big.Int) common.Address {
	_, tx, _, err := vrf_beacon_consumer.DeployBeaconVRFConsumer(e.Owner, e.Ec, common.HexToAddress(coordinatorAddress), shouldFail, beaconPeriodBlocks)
	helpers.PanicErr(err)
	return helpers.ConfirmContractDeployed(context.Background(), e.Ec, tx, e.ChainID)
}

func addClientToDKG(e helpers.Environment, dkgAddress string, keyID string, clientAddress string) {
	keyIDBytes := decodeHexTo32ByteArray(keyID)

	dkg, err := dkgContract.NewDKG(common.HexToAddress(dkgAddress), e.Ec)
	helpers.PanicErr(err)

	tx, err := dkg.AddClient(e.Owner, keyIDBytes, common.HexToAddress(clientAddress))
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func removeClientFromDKG(e helpers.Environment, dkgAddress string, keyID string, clientAddress string) {
	keyIDBytes := decodeHexTo32ByteArray(keyID)

	dkg, err := dkgContract.NewDKG(common.HexToAddress(dkgAddress), e.Ec)
	helpers.PanicErr(err)

	tx, err := dkg.RemoveClient(e.Owner, keyIDBytes, common.HexToAddress(clientAddress))
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func setDKGConfig(e helpers.Environment, dkgAddress string, c dkgSetConfigArgs) {
	oracleIdentities := toOraclesIdentityList(
		helpers.ParseAddressSlice(c.onchainPubKeys),
		strings.Split(c.offchainPubKeys, ","),
		strings.Split(c.configPubKeys, ","),
		strings.Split(c.peerIDs, ","),
		strings.Split(c.transmitters, ","))

	ed25519Suite := edwards25519.NewBlakeSHA256Ed25519()
	var signingKeys []kyber.Point
	for _, signingKey := range strings.Split(c.dkgSigningPubKeys, ",") {
		signingKeyBytes, err := hex.DecodeString(signingKey)
		helpers.PanicErr(err)
		signingKeyPoint := ed25519Suite.Point()
		helpers.PanicErr(signingKeyPoint.UnmarshalBinary(signingKeyBytes))
		signingKeys = append(signingKeys, signingKeyPoint)
	}

	altbn128Suite := &altbn_128.PairingSuite{}
	var encryptionKeys []kyber.Point
	for _, encryptionKey := range strings.Split(c.dkgEncryptionPubKeys, ",") {
		encryptionKeyBytes, err := hex.DecodeString(encryptionKey)
		helpers.PanicErr(err)
		encryptionKeyPoint := altbn128Suite.G1().Point()
		helpers.PanicErr(encryptionKeyPoint.UnmarshalBinary(encryptionKeyBytes))
		encryptionKeys = append(encryptionKeys, encryptionKeyPoint)
	}

	keyIDBytes := decodeHexTo32ByteArray(c.keyID)

	offchainConfig, err := dkg.OffchainConfig(dkg.EncryptionPublicKeys(encryptionKeys), dkg.SigningPublicKeys(signingKeys), &altbn_128.G1{}, &ocr2vrftypes.PairingTranslation{
		&altbn_128.PairingSuite{},
	})
	helpers.PanicErr(err)
	onchainConfig, err := dkg.OnchainConfig(dkg.KeyID(keyIDBytes))
	helpers.PanicErr(err)

	fmt.Println("dkg offchain config:", hex.EncodeToString(offchainConfig))
	fmt.Println("dkg onchain config:", hex.EncodeToString(onchainConfig))

	_, _, f, onchainConfig, offchainConfigVersion, offchainConfig, err := confighelper.ContractSetConfigArgsForTests(
		c.deltaProgress,
		c.deltaResend,
		c.deltaRound,
		c.deltaGrace,
		c.deltaStage,
		c.maxRounds,
		helpers.ParseIntSlice(c.schedule),
		oracleIdentities,
		offchainConfig,
		c.maxDurationQuery,
		c.maxDurationObservation,
		c.maxDurationReport,
		c.maxDurationAccept,
		c.maxDurationTransmit,
		int(c.f),
		onchainConfig)

	helpers.PanicErr(err)

	dkg := newDKG(common.HexToAddress(dkgAddress), e.Ec)

	tx, err := dkg.SetConfig(e.Owner, helpers.ParseAddressSlice(c.onchainPubKeys), helpers.ParseAddressSlice(c.transmitters), f, onchainConfig, offchainConfigVersion, offchainConfig)
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func setVRFBeaconCoordinatorConfig(e helpers.Environment, vrfBeaconCoordinatorAddr string, c vrfBeaconCoordinatorSetConfigArgs) {
	oracleIdentities := toOraclesIdentityList(
		helpers.ParseAddressSlice(c.onchainPubKeys),
		strings.Split(c.offchainPubKeys, ","),
		strings.Split(c.configPubKeys, ","),
		strings.Split(c.peerIDs, ","),
		strings.Split(c.transmitters, ","))

	keyIDBytes := decodeHexTo32ByteArray(c.keyID)

	offchainConfig := ocr2vrf.OffchainConfig(keyIDBytes)

	confDelays := make(map[uint32]struct{})
	for _, c := range strings.Split(c.confDelays, ",") {
		confDelay, err := strconv.ParseUint(c, 0, 32)
		helpers.PanicErr(err)
		confDelays[uint32(confDelay)] = struct{}{}
	}

	onchainConfig := ocr2vrf.OnchainConfig(confDelays)

	_, _, f, onchainConfig, offchainConfigVersion, offchainConfig, err := confighelper.ContractSetConfigArgsForTests(
		c.deltaProgress,
		c.deltaResend,
		c.deltaRound,
		c.deltaGrace,
		c.deltaStage,
		c.maxRounds,
		helpers.ParseIntSlice(c.schedule),
		oracleIdentities,
		offchainConfig,
		c.maxDurationQuery,
		c.maxDurationObservation,
		c.maxDurationReport,
		c.maxDurationAccept,
		c.maxDurationTransmit,
		int(c.f),
		onchainConfig)

	helpers.PanicErr(err)

	coordinator := newVRFBeaconCoordinator(common.HexToAddress(vrfBeaconCoordinatorAddr), e.Ec)

	tx, err := coordinator.SetConfig(e.Owner, helpers.ParseAddressSlice(c.onchainPubKeys), helpers.ParseAddressSlice(c.transmitters), f, onchainConfig, offchainConfigVersion, offchainConfig)
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func toOraclesIdentityList(onchainPubKeys []common.Address, offchainPubKeys, configPubKeys, peerIDs, transmitters []string) []confighelper.OracleIdentityExtra {
	offchainPubKeysBytes := []types.OffchainPublicKey{}
	for _, pkHex := range offchainPubKeys {
		pkBytes, err := hex.DecodeString(pkHex)
		helpers.PanicErr(err)
		pkBytesFixed := [ed25519.PublicKeySize]byte{}
		n := copy(pkBytesFixed[:], pkBytes)
		if n != ed25519.PublicKeySize {
			panic("wrong num elements copied")
		}

		offchainPubKeysBytes = append(offchainPubKeysBytes, types.OffchainPublicKey(pkBytesFixed))
	}

	configPubKeysBytes := []types.ConfigEncryptionPublicKey{}
	for _, pkHex := range configPubKeys {
		pkBytes, err := hex.DecodeString(pkHex)
		helpers.PanicErr(err)

		pkBytesFixed := [ed25519.PublicKeySize]byte{}
		n := copy(pkBytesFixed[:], pkBytes)
		if n != ed25519.PublicKeySize {
			panic("wrong num elements copied")
		}

		configPubKeysBytes = append(configPubKeysBytes, types.ConfigEncryptionPublicKey(pkBytesFixed))
	}

	o := []confighelper.OracleIdentityExtra{}
	for index := range configPubKeys {
		o = append(o, confighelper.OracleIdentityExtra{
			OracleIdentity: confighelper.OracleIdentity{
				OnchainPublicKey:  onchainPubKeys[index][:],
				OffchainPublicKey: offchainPubKeysBytes[index],
				PeerID:            peerIDs[index],
				TransmitAccount:   types.Account(transmitters[index]),
			},
			ConfigEncryptionPublicKey: configPubKeysBytes[index],
		})
	}
	return o
}

func requestRandomness(e helpers.Environment, coordinatorAddress string, numWords uint16, subID uint64, confDelay *big.Int) {
	coordinator := newVRFBeaconCoordinator(common.HexToAddress(coordinatorAddress), e.Ec)

	tx, err := coordinator.RequestRandomness(e.Owner, numWords, subID, confDelay)
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func getRandomness(e helpers.Environment, coordinatorAddress string, requestID *big.Int) {
	coordinator := newVRFBeaconCoordinator(common.HexToAddress(coordinatorAddress), e.Ec)

	tx, err := coordinator.GetRandomness(e.Owner, requestID)
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func requestRandomnessFromConsumer(e helpers.Environment, consumerAddress string, numWords uint16, subID uint64, confDelay *big.Int) *big.Int {
	consumer := newVRFBeaconCoordinatorConsumer(common.HexToAddress(consumerAddress), e.Ec)

	tx, err := consumer.TestRequestRandomness(e.Owner, numWords, subID, confDelay)
	helpers.PanicErr(err)
	receipt := helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)

	periodBlocks, err := consumer.IBeaconPeriodBlocks(nil)
	helpers.PanicErr(err)

	blockNumber := receipt.BlockNumber
	periodOffset := new(big.Int).Mod(blockNumber, periodBlocks)
	nextBeaconOutputHeight := new(big.Int).Sub(new(big.Int).Add(blockNumber, periodBlocks), periodOffset)

	fmt.Println("nextBeaconOutputHeight: ", nextBeaconOutputHeight)

	requestID, err := consumer.SRequestsIDs(nil, nextBeaconOutputHeight, confDelay)

	fmt.Println("requestID: ", requestID)

	return requestID
}

func requestRandomnessCallback(
	e helpers.Environment,
	consumerAddress string,
	numWords uint16,
	subID uint64,
	confDelay *big.Int,
	callbackGasLimit uint32,
	args []byte,
) (requestID *big.Int) {
	consumer := newVRFBeaconCoordinatorConsumer(common.HexToAddress(consumerAddress), e.Ec)

	tx, err := consumer.TestRequestRandomnessFulfillment(e.Owner, subID, numWords, confDelay, callbackGasLimit, args)
	helpers.PanicErr(err)
	receipt := helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID, "TestRequestRandomnessFulfillment")

	periodBlocks, err := consumer.IBeaconPeriodBlocks(nil)
	helpers.PanicErr(err)

	blockNumber := receipt.BlockNumber
	periodOffset := new(big.Int).Mod(blockNumber, periodBlocks)
	nextBeaconOutputHeight := new(big.Int).Sub(new(big.Int).Add(blockNumber, periodBlocks), periodOffset)

	fmt.Println("nextBeaconOutputHeight: ", nextBeaconOutputHeight)

	requestID, err = consumer.SRequestsIDs(nil, nextBeaconOutputHeight, confDelay)

	fmt.Println("requestID: ", requestID)

	return requestID
}

func getRandomnessFromConsumer(e helpers.Environment, consumerAddress string, requestID *big.Int) {
	consumer := newVRFBeaconCoordinatorConsumer(common.HexToAddress(consumerAddress), e.Ec)

	tx, err := consumer.TestGetRandomness(e.Owner, requestID)
	helpers.PanicErr(err)
	helpers.ConfirmTXMined(context.Background(), e.Ec, tx, e.ChainID)
}

func newVRFBeaconCoordinator(addr common.Address, client *ethclient.Client) *vrf_beacon_coordinator.VRFBeaconCoordinator {
	coordinator, err := vrf_beacon_coordinator.NewVRFBeaconCoordinator(addr, client)
	helpers.PanicErr(err)
	return coordinator
}

func newDKG(addr common.Address, client *ethclient.Client) *dkgContract.DKG {
	dkg, err := dkgContract.NewDKG(addr, client)
	helpers.PanicErr(err)
	return dkg
}

func newVRFBeaconCoordinatorConsumer(addr common.Address, client *ethclient.Client) *vrf_beacon_consumer.BeaconVRFConsumer {
	consumer, err := vrf_beacon_consumer.NewBeaconVRFConsumer(addr, client)
	helpers.PanicErr(err)
	return consumer
}

func decodeHexTo32ByteArray(val string) (byteArray [32]byte) {
	decoded, err := hex.DecodeString(val)
	helpers.PanicErr(err)
	if len(decoded) != 32 {
		panic(fmt.Sprintf("expected value to be 32 bytes but received %d bytes", len(decoded)))
	}
	copy(byteArray[:], decoded)
	return
}

func setupOCR2VRFNodeFromClient(client *cmd.Client, context *cli.Context) *cmd.SetupOCR2VRFNodePayload {
	payload, err := client.ConfigureOCR2VRFNode(context)
	helpers.PanicErr(err)

	return payload
}

func configureEnvironmentVariables() {
	helpers.PanicErr(os.Setenv("FEATURE_OFFCHAIN_REPORTING2", "true"))
	helpers.PanicErr(os.Setenv("SKIP_DATABASE_PASSWORD_COMPLEXITY_CHECK", "true"))
}

func resetDatabase(client *cmd.Client, context *cli.Context, index int, databasePrefix string, databaseSuffixes string) {
	helpers.PanicErr(os.Setenv("DATABASE_URL", fmt.Sprintf("%s-%d?%s", databasePrefix, index, databaseSuffixes)))
	helpers.PanicErr(client.ResetDatabase(context))
}

func newSetupClient() *cmd.Client {
	lggr, closeLggr := logger.NewLogger()
	cfg := config.NewGeneralConfig(lggr)

	prompter := cmd.NewTerminalPrompter()
	return &cmd.Client{
		Renderer:                       cmd.RendererTable{Writer: os.Stdout},
		Config:                         cfg,
		Logger:                         lggr,
		CloseLogger:                    closeLggr,
		AppFactory:                     cmd.ChainlinkAppFactory{},
		KeyStoreAuthenticator:          cmd.TerminalKeyStoreAuthenticator{Prompter: prompter},
		FallbackAPIInitializer:         cmd.NewPromptingAPIInitializer(prompter, lggr),
		Runner:                         cmd.ChainlinkRunner{},
		PromptingSessionRequestBuilder: cmd.NewPromptingSessionRequestBuilder(prompter),
		ChangePasswordPrompter:         cmd.NewChangePasswordPrompter(),
		PasswordPrompter:               cmd.NewPasswordPrompter(),
	}
}

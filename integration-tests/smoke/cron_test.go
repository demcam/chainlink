package smoke

//revive:disable:dot-imports
import (
	"fmt"
	"strings"

	"github.com/smartcontractkit/chainlink-env/environment"
	"github.com/smartcontractkit/chainlink-env/pkg/helm/chainlink"
	"github.com/smartcontractkit/chainlink-env/pkg/helm/ethereum"
	"github.com/smartcontractkit/chainlink-env/pkg/helm/mockserver"
	mockservercfg "github.com/smartcontractkit/chainlink-env/pkg/helm/mockserver-cfg"
	"github.com/smartcontractkit/chainlink-testing-framework/blockchain"
	ctfClient "github.com/smartcontractkit/chainlink-testing-framework/client"
	"github.com/smartcontractkit/chainlink-testing-framework/utils"
	networks "github.com/smartcontractkit/chainlink/integration-tests"
	"github.com/smartcontractkit/chainlink/integration-tests/actions"
	"github.com/smartcontractkit/chainlink/integration-tests/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	uuid "github.com/satori/go.uuid"
)

var _ = Describe("Cronjob suite @cron", func() {
	var (
		testScenarios = []TableEntry{
			Entry("Cronjob suite on Simulated Network @simulated", networks.SimulatedEVM),
			Entry("Cronjob suite on General EVM Network read from env vars @general", networks.GeneralEVM()),
			Entry("Cronjob suite on Metis Stardust @metis", networks.MetisStardust),
			Entry("Cronjob suite on Sepolia Testnet @sepolia", networks.SepoliaTestnet),
			Entry("Cronjob suite on Görli Testnet @goerli", networks.GoerliTestnet),
			Entry("Cronjob suite on Klaytn Baobab @klaytn", networks.KlaytnBaobab),
		}

		err             error
		job             *client.Job
		chainlinkNode   client.Chainlink
		mockServer      *ctfClient.MockserverClient
		testEnvironment *environment.Environment
	)

	AfterEach(func() {
		By("Tearing down the environment")
		err = actions.TeardownSuite(testEnvironment, utils.ProjectRoot, []client.Chainlink{chainlinkNode}, nil, nil)
		Expect(err).ShouldNot(HaveOccurred(), "Environment teardown shouldn't fail")
	})

	DescribeTable("Cronjob suite on different EVM networks", func(
		testNetwork *blockchain.EVMNetwork,
	) {
		evmChart := ethereum.New(nil)
		if !testNetwork.Simulated {
			evmChart = ethereum.New(&ethereum.Props{
				NetworkName: testNetwork.Name,
				Simulated:   testNetwork.Simulated,
				WsURLs:      testNetwork.URLs,
			})
		}
		By("Deploying the environment")
		testEnvironment = environment.New(&environment.Config{
			NamespacePrefix: fmt.Sprintf("smoke-cron-%s", strings.ReplaceAll(strings.ToLower(testNetwork.Name), " ", "-")),
		}).
			AddHelm(mockservercfg.New(nil)).
			AddHelm(mockserver.New(nil)).
			AddHelm(evmChart).
			AddHelm(chainlink.New(0, map[string]interface{}{
				"env": testNetwork.ChainlinkValuesMap(),
			}))
		err = testEnvironment.Run()
		Expect(err).ShouldNot(HaveOccurred(), "Error deploying test environment")

		By("Connecting to launched resources")
		cls, err := client.ConnectChainlinkNodes(testEnvironment)
		Expect(err).ShouldNot(HaveOccurred(), "Connecting to chainlink nodes shouldn't fail")
		mockServer, err = ctfClient.ConnectMockServer(testEnvironment)
		Expect(err).ShouldNot(HaveOccurred(), "Creating mockserver client shouldn't fail")
		chainlinkNode = cls[0]

		By("Adding cron job to a node")
		err = mockServer.SetValuePath("/variable", 5)
		Expect(err).ShouldNot(HaveOccurred(), "Setting value path in mockserver shouldn't fail")

		bta := client.BridgeTypeAttributes{
			Name:        fmt.Sprintf("variable-%s", uuid.NewV4().String()),
			URL:         fmt.Sprintf("%s/variable", mockServer.Config.ClusterURL),
			RequestData: "{}",
		}
		err = chainlinkNode.CreateBridge(&bta)
		Expect(err).ShouldNot(HaveOccurred(), "Creating bridge in chainlink node shouldn't fail")

		job, err = chainlinkNode.CreateJob(&client.CronJobSpec{
			Schedule:          "CRON_TZ=UTC * * * * * *",
			ObservationSource: client.ObservationSourceSpecBridge(bta),
		})
		Expect(err).ShouldNot(HaveOccurred(), "Creating Cron Job in chainlink node shouldn't fail")

		Eventually(func(g Gomega) {
			jobRuns, err := chainlinkNode.ReadRunsByJob(job.Data.ID)
			g.Expect(err).ShouldNot(HaveOccurred(), "Reading Job run data shouldn't fail")

			g.Expect(len(jobRuns.Data)).Should(BeNumerically(">=", 5), "Expected number of job runs to be greater than 5, but got %d", len(jobRuns.Data))

			for _, jr := range jobRuns.Data {
				g.Expect(jr.Attributes.Errors).Should(Equal([]interface{}{nil}), "Job run %s shouldn't have errors", jr.ID)
			}
		}, "2m", "1s").Should(Succeed())
	},
		testScenarios,
	)
})

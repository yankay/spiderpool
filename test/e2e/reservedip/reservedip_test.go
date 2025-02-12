// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0
package reservedip_test

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spidernet-io/e2eframework/tools"
	"github.com/spidernet-io/spiderpool/pkg/constant"
	spiderpool "github.com/spidernet-io/spiderpool/pkg/k8s/apis/v1"
	"github.com/spidernet-io/spiderpool/pkg/types"
	"github.com/spidernet-io/spiderpool/test/e2e/common"
)

var _ = Describe("test reservedIP", Label("reservedIP"), func() {
	var nsName, DeployName, v4PoolName, v6PoolName, v4ReservedIpName, v6ReservedIpName, nic, podAnnoStr string
	var v4PoolNameList, v6PoolNameList []string
	var iPv4PoolObj, iPv6PoolObj *spiderpool.IPPool
	var v4ReservedIpObj, v6ReservedIpObj *spiderpool.ReservedIP
	var err error

	BeforeEach(func() {
		nic = "eth0"
		//Init namespace name and create
		nsName = "ns" + tools.RandomName()
		GinkgoWriter.Printf("Try to create namespace %v \n", nsName)
		err := frame.CreateNamespace(nsName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace %v", nsName)
		// Init test Deployment/Pod name
		DeployName = "sr-pod" + tools.RandomName()

		if frame.Info.IpV4Enabled {
			v4PoolName, iPv4PoolObj = common.GenerateExampleIpv4poolObject(3)
			err := common.CreateIppool(frame, iPv4PoolObj)
			Expect(err).NotTo(HaveOccurred(), "Failed to create v4 Pool %v \n", v4PoolName)
			GinkgoWriter.Printf("Successfully created v4 Pool: %v \n", v4PoolName)
			v4PoolNameList = append(v4PoolNameList, v4PoolName)
		}

		if frame.Info.IpV6Enabled {
			v6PoolName, iPv6PoolObj = common.GenerateExampleIpv6poolObject(1)
			err := common.CreateIppool(frame, iPv6PoolObj)
			Expect(err).NotTo(HaveOccurred(), "Failed to create v6 Pool %v \n", v6PoolName)
			GinkgoWriter.Printf("Successfully created v6 Pool: %v \n", v6PoolName)
			v6PoolNameList = append(v6PoolNameList, v6PoolName)
		}
		// Clean test env
		DeferCleanup(func() {
			GinkgoWriter.Printf("Delete namespace %v \n", nsName)
			err = frame.DeleteNamespace(nsName)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete namespace %v", nsName)

			if frame.Info.IpV4Enabled {
				err := common.DeleteIPPoolByName(frame, v4PoolName)
				Expect(err).NotTo(HaveOccurred())
			}
			if frame.Info.IpV6Enabled {
				err := common.DeleteIPPoolByName(frame, v6PoolName)
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	It("an IP who is set in ReservedIP CRD, should not be assigned to a pod", Label("S00001", "smoke"), func() {

		if frame.Info.IpV4Enabled {
			v4ReservedIpName, v4ReservedIpObj = common.GenerateExampleV4ReservedIpObject(iPv4PoolObj.Spec.IPs)
			err = common.CreateReservedIP(frame, v4ReservedIpObj)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Successfully created v4 reservedIP: %v \n", v4ReservedIpName)
		}

		if frame.Info.IpV6Enabled {
			v6ReservedIpName, v6ReservedIpObj = common.GenerateExampleV6ReservedIpObject(iPv6PoolObj.Spec.IPs)
			err = common.CreateReservedIP(frame, v6ReservedIpObj)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Successfully created v6 reservedIP : %v \n", v6ReservedIpName)
		}

		podAnno := types.AnnoPodIPPoolValue{
			NIC:       &nic,
			IPv4Pools: v4PoolNameList,
			IPv6Pools: v6PoolNameList,
		}
		b, e := json.Marshal(podAnno)
		Expect(e).NotTo(HaveOccurred())
		podAnnoStr = string(b)

		// Generate deployment yaml and annotation
		deployObject := common.GenerateExampleDeploymentYaml(DeployName, nsName, int32(1))
		deployObject.Spec.Template.Annotations = map[string]string{constant.AnnoPodIPPool: podAnnoStr}
		GinkgoWriter.Printf("Try to create Deployment: %v/%v with annotation %v = %v \n", nsName, DeployName, constant.AnnoPodIPPool, podAnnoStr)
		ctx1, cancel1 := context.WithTimeout(context.Background(), time.Minute)
		defer cancel1()
		// Try to create a deployment and wait for replicas to meet expectations（because the pod is not running）
		podlist, err := common.CreateDeployUntilExpectedReplicas(frame, deployObject, ctx1)
		Expect(err).NotTo(HaveOccurred())
		Expect(int32(len(podlist.Items))).Should(Equal(*deployObject.Spec.Replicas))

		// Get the Pod creation failure Event
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, pod := range podlist.Items {
			Expect(frame.WaitExceptEventOccurred(ctx, common.PodEventKind, pod.Name, pod.Namespace, common.CNIFailedToSetUpNetwork)).To(Succeed())
			Expect(err).NotTo(HaveOccurred(), "Failed to get Event %v/%v = %v \n", pod.Namespace, pod.Name, common.CNIFailedToSetUpNetwork)
			GinkgoWriter.Printf("IP assignment for Deployment/Pod: %v/%v fails when an IP is set in the ReservedIP CRD. \n", pod.Namespace, pod.Name)
		}

		// Try to delete reservedIP
		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Minute)
		defer cancel2()
		if frame.Info.IpV4Enabled {
			Expect(common.DeleteResverdIPUntilFinish(ctx2, frame, v4ReservedIpName)).To(Succeed())
			GinkgoWriter.Printf("Delete v4 reservedIP: %v successfully \n", v4ReservedIpName)
		}
		if frame.Info.IpV6Enabled {
			Expect(common.DeleteResverdIPUntilFinish(ctx2, frame, v6ReservedIpName)).To(Succeed())
			GinkgoWriter.Printf("Delete v6 reservedIP: %v successfully \n", v6ReservedIpName)
		}

		// After removing the reservedIP, wait for the Pod to restart until running
		err = frame.RestartDeploymentPodUntilReady(DeployName, nsName, time.Minute)
		Expect(err).NotTo(HaveOccurred())

		// Get the list of Deployments/Pod after reboot
		podlist, err = frame.GetPodListByLabel(deployObject.Spec.Selector.MatchLabels)
		Expect(err).NotTo(HaveOccurred())
		Expect(int32(len(podlist.Items))).Should(Equal(*deployObject.Spec.Replicas))

		// Succeeded to assign IPv4、IPv6 ip for Pod
		err = frame.CheckPodListIpReady(podlist)
		Expect(err).NotTo(HaveOccurred(), "Failed to check IPv4 or IPv6 ")
		GinkgoWriter.Printf("Succeeded to assign IPv4、IPv6 ip for Pod %v/%v \n", nsName, DeployName)

		// Check Deployment Pod IP recorded in IPPool
		ok, _, _, e := common.CheckPodIpRecordInIppool(frame, v4PoolNameList, v6PoolNameList, podlist)
		Expect(e).NotTo(HaveOccurred(), "Failed to check Pod IP record in IPPool")
		Expect(ok).To(BeTrue())
		GinkgoWriter.Printf("Deployment/Pod: %v/%v IP recorded in IPPool %v,%v \n", nsName, DeployName, v4PoolNameList, v6PoolNameList)

		// Try to delete Deployment
		Expect(frame.DeleteDeploymentUntilFinish(DeployName, nsName, time.Minute)).To(Succeed())
		GinkgoWriter.Printf("Delete Deployment: %v/%v successfully \n", nsName, DeployName)

		// Check if the Pod IP in IPPool reclaimed normally
		Expect(common.WaitIPReclaimedFinish(frame, v4PoolNameList, v6PoolNameList, podlist, time.Minute)).To(Succeed())
		GinkgoWriter.Println("Deployment/Pod IP successfully released")
	})
})

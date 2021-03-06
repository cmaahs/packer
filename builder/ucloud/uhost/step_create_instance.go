package uhost

import (
	"context"
	"fmt"
	"github.com/hashicorp/packer/common/retry"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
	"github.com/ucloud/ucloud-sdk-go/services/uhost"
	"github.com/ucloud/ucloud-sdk-go/ucloud"
	"math/rand"
	"strings"
	"time"
)

type stepCreateInstance struct {
	Region        string
	Zone          string
	InstanceType  string
	InstanceName  string
	BootDiskType  string
	SourceImageId string
	UsePrivateIp  bool

	instanceId string
}

func (s *stepCreateInstance) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("client").(*UCloudClient)
	conn := client.uhostconn
	ui := state.Get("ui").(packer.Ui)

	ui.Say("Creating Instance...")
	resp, err := conn.CreateUHostInstance(s.buildCreateInstanceRequest(state))
	if err != nil {
		return halt(state, err, "Error on creating instance")
	}
	instanceId := resp.UHostIds[0]

	err = retry.Config{
		Tries: 20,
		ShouldRetry: func(err error) bool {
			return isExpectedStateError(err)
		},
		RetryDelay: (&retry.Backoff{InitialBackoff: 2 * time.Second, MaxBackoff: 6 * time.Second, Multiplier: 2}).Linear,
	}.Run(ctx, func(ctx context.Context) error {
		inst, err := client.describeUHostById(instanceId)
		if err != nil {
			return err
		}

		if inst.State == "ResizeFail" {
			return fmt.Errorf("resizing instance failed")
		}

		if inst.State == "Install Fail" {
			return fmt.Errorf("install failed")
		}

		if inst == nil || inst.State != instanceStateRunning {
			return newExpectedStateError("instance", instanceId)
		}

		return nil
	})

	if err != nil {
		return halt(state, err, fmt.Sprintf("Error on waiting for instance %q to become available", instanceId))
	}

	ui.Message(fmt.Sprintf("Creating instance %q complete", instanceId))
	instance, err := client.describeUHostById(instanceId)
	if err != nil {
		return halt(state, err, fmt.Sprintf("Error on reading instance when creating %q", instanceId))
	}

	s.instanceId = instanceId
	state.Put("instance", instance)

	if instance.BootDiskState != bootDiskStateNormal {
		ui.Say("Waiting for boot disk of instance initialized")
		if s.BootDiskType == "local_normal" || s.BootDiskType == "local_ssd" {
			ui.Message(fmt.Sprintf("Warning: It takes around 10 mins for boot disk initialization when `boot_disk_type` is %q", s.BootDiskType))
		}

		err = retry.Config{
			Tries: 200,
			ShouldRetry: func(err error) bool {
				return isExpectedStateError(err)
			},
			RetryDelay: (&retry.Backoff{InitialBackoff: 2 * time.Second, MaxBackoff: 12 * time.Second, Multiplier: 2}).Linear,
		}.Run(ctx, func(ctx context.Context) error {
			inst, err := client.describeUHostById(instanceId)
			if err != nil {
				return err
			}
			if inst.BootDiskState != bootDiskStateNormal {
				return newExpectedStateError("boot_disk of instance", instanceId)
			}

			return nil
		})

		if err != nil {
			return halt(state, err, fmt.Sprintf("Error on waiting for boot disk of instance %q initialized", instanceId))
		}

		ui.Message(fmt.Sprintf("Waiting for boot disk of instance %q initialized complete", instanceId))
	}

	return multistep.ActionContinue
}

func (s *stepCreateInstance) Cleanup(state multistep.StateBag) {
	if s.instanceId == "" {
		return
	}
	_, cancelled := state.GetOk(multistep.StateCancelled)
	_, halted := state.GetOk(multistep.StateHalted)

	ui := state.Get("ui").(packer.Ui)
	ctx := context.TODO()

	if cancelled || halted {
		ui.Say("Deleting instance because of cancellation or error...")
	} else {
		ui.Say("Deleting instance...")
	}

	client := state.Get("client").(*UCloudClient)
	conn := client.uhostconn

	instance, err := client.describeUHostById(s.instanceId)
	if err != nil {
		if isNotFoundError(err) {
			return
		}
		ui.Error(fmt.Sprintf("Error on reading instance when deleting %q, %s",
			s.instanceId, err.Error()))
		return
	}

	if instance.State != instanceStateStopped {
		stopReq := conn.NewStopUHostInstanceRequest()
		stopReq.UHostId = ucloud.String(s.instanceId)
		if _, err = conn.StopUHostInstance(stopReq); err != nil {
			ui.Error(fmt.Sprintf("Error on stopping instance when deleting %q, %s",
				s.instanceId, err.Error()))
			return
		}

		err = retry.Config{
			Tries: 30,
			ShouldRetry: func(err error) bool {
				return isExpectedStateError(err)
			},
			RetryDelay: (&retry.Backoff{InitialBackoff: 2 * time.Second, MaxBackoff: 6 * time.Second, Multiplier: 2}).Linear,
		}.Run(ctx, func(ctx context.Context) error {
			instance, err := client.describeUHostById(s.instanceId)
			if err != nil {
				return err
			}

			if instance.State != instanceStateStopped {
				return newExpectedStateError("instance", s.instanceId)
			}

			return nil
		})

		if err != nil {
			ui.Error(fmt.Sprintf("Error on waiting for stopping instance when deleting %q, %s",
				s.instanceId, err.Error()))
			return
		}
	}

	deleteReq := conn.NewTerminateUHostInstanceRequest()
	deleteReq.UHostId = ucloud.String(s.instanceId)
	deleteReq.ReleaseUDisk = ucloud.Bool(true)
	deleteReq.ReleaseEIP = ucloud.Bool(true)

	if _, err = conn.TerminateUHostInstance(deleteReq); err != nil {
		ui.Error(fmt.Sprintf("Error on deleting instance %q, %s",
			s.instanceId, err.Error()))
		return
	}

	err = retry.Config{
		Tries:       30,
		ShouldRetry: func(err error) bool { return !isNotFoundError(err) },
		RetryDelay:  (&retry.Backoff{InitialBackoff: 2 * time.Second, MaxBackoff: 6 * time.Second, Multiplier: 2}).Linear,
	}.Run(ctx, func(ctx context.Context) error {
		_, err := client.describeUHostById(s.instanceId)
		return err
	})

	if err != nil {
		ui.Error(fmt.Sprintf("Error on waiting for instance %q to be deleted: %s",
			s.instanceId, err.Error()))
		return
	}

	ui.Message(fmt.Sprintf("Deleting instance %q complete", s.instanceId))
}

func (s *stepCreateInstance) buildCreateInstanceRequest(state multistep.StateBag) *uhost.CreateUHostInstanceRequest {
	client := state.Get("client").(*UCloudClient)
	conn := client.uhostconn
	srcImage := state.Get("source_image").(*uhost.UHostImageSet)
	config := state.Get("config").(*Config)
	connectConfig := &config.RunConfig.Comm

	var password string
	if srcImage.OsType == "Linux" {
		password = config.Comm.SSHPassword
	}

	if password == "" {
		password = fmt.Sprintf("%s%s%s",
			s.randStringFromCharSet(5, defaultPasswordStr),
			s.randStringFromCharSet(1, defaultPasswordSpe),
			s.randStringFromCharSet(5, defaultPasswordNum))
		if srcImage.OsType == "Linux" {
			connectConfig.SSHPassword = password
		}
	}

	req := conn.NewCreateUHostInstanceRequest()
	t, _ := parseInstanceType(s.InstanceType)

	req.CPU = ucloud.Int(t.CPU)
	req.Memory = ucloud.Int(t.Memory)
	req.Name = ucloud.String(s.InstanceName)
	req.LoginMode = ucloud.String("Password")
	req.Zone = ucloud.String(s.Zone)
	req.ImageId = ucloud.String(s.SourceImageId)
	req.ChargeType = ucloud.String("Dynamic")
	req.Password = ucloud.String(password)

	if v, ok := state.GetOk("security_group_id"); ok {
		req.SecurityGroupId = ucloud.String(v.(string))
	}

	if v, ok := state.GetOk("vpc_id"); ok {
		req.VPCId = ucloud.String(v.(string))
	}

	if v, ok := state.GetOk("subnet_id"); ok {
		req.SubnetId = ucloud.String(v.(string))
	}

	bootDisk := uhost.UHostDisk{}
	bootDisk.IsBoot = ucloud.String("true")
	bootDisk.Size = ucloud.Int(srcImage.ImageSize)
	bootDisk.Type = ucloud.String(bootDiskTypeMap[s.BootDiskType])

	req.Disks = append(req.Disks, bootDisk)

	if !s.UsePrivateIp {
		operatorName := ucloud.String("International")
		if strings.HasPrefix(s.Region, "cn-") {
			operatorName = ucloud.String("Bgp")
		}
		networkInterface := uhost.CreateUHostInstanceParamNetworkInterface{
			EIP: &uhost.CreateUHostInstanceParamNetworkInterfaceEIP{
				Bandwidth:    ucloud.Int(30),
				PayMode:      ucloud.String("Traffic"),
				OperatorName: operatorName,
			},
		}

		req.NetworkInterface = append(req.NetworkInterface, networkInterface)
	}
	return req
}

func (s *stepCreateInstance) randStringFromCharSet(strlen int, charSet string) string {
	rand.Seed(time.Now().UTC().UnixNano())
	result := make([]byte, strlen)
	for i := 0; i < strlen; i++ {
		result[i] = charSet[rand.Intn(len(charSet))]
	}
	return string(result)
}

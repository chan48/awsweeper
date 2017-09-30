package main

import (
	"strings"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/hashicorp/terraform/terraform"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/route53"
	"fmt"
	"github.com/aws/aws-sdk-go/service/efs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/kms"
	"regexp"
	"gopkg.in/yaml.v2"
	"log"
	"io/ioutil"
	_yaml2 "github.com/ghodss/yaml"
	"encoding/json"
	"github.com/jmoiron/jsonq"
	"strconv"
	"reflect"
)

type OutputCommand struct {
	ec2conn         *ec2.EC2
	autoscalingconn *autoscaling.AutoScaling
	elbconn         *elb.ELB
	r53conn         *route53.Route53
	cfconn          *cloudformation.CloudFormation
	efsconn         *efs.EFS
	iamconn         *iam.IAM
	kmsconn         *kms.KMS
	provider        *terraform.ResourceProvider
	resourceTypes   []string
	filter          []*ec2.Filter
	yamlConfig      map[string]B
	out             map[string]B
	deleteDescriptions          []ResourceDeleteDescription
}

func (c *WipeCommand) Run(args []string) int {

	if len(args) > 0 {
		yamlFile, err := ioutil.ReadFile(args[0])
		check(err)
		err = yaml.Unmarshal([]byte(yamlFile), &c.yamlConfig)
		check(err)

		for _, d := range c.deleteDescriptions {
			c.listStuff(d, Invoke(d.Describe, d.Input))
		}
	} else {
		fmt.Println(c.Help())
		return 1
	}

	d, err := yaml.Marshal(&c.yamlConfig)
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	d1 := []byte(string(d))
	err = ioutil.WriteFile("out.yaml", d1, 0644)
	check(err)

	return 0
}

func Invoke(fn interface{}, args ...interface{}) interface {} {
	v := reflect.ValueOf(fn)
	rargs := make([]reflect.Value, len(args))
	for i, a := range args {
		rargs[i] = reflect.ValueOf(a)
	}
	result := v.Call(rargs)
	return result[0].Interface()
}

func (c *WipeCommand) Help() string {
	helpText := `
Usage: awsweeper output <file.yaml>

Lists all infrastructure of your AWS account in <file.yaml>.

Currently supported resource types are:
`

	for _, k := range c.resourceTypes {
		helpText += fmt.Sprintf("\t\t%s\n", k)
	}

	return strings.TrimSpace(helpText)
}

func (c *WipeCommand) Synopsis() string {
	return "Delete all or one specific resource type"
}

func (c *WipeCommand) deleteKmsAliases(resourceType ResourceDeleteDescription) {
	res, err := c.kmsconn.ListAliases(&kms.ListAliasesInput{})

	if err == nil {
		c.listStuff(resourceType, res)
	}
}

func (c *WipeCommand) deleteRoute53Record(resourceType string, hostedZoneId *string) {
	res, err := c.r53conn.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{
		HostedZoneId: hostedZoneId,
	})

	if err == nil {
		ids := []*string{}

		for _, r := range res.ResourceRecordSets {
			for _, rr := range r.ResourceRecords {
				if c.checkDelete(resourceType, rr.Value) {
					ids = append(ids, rr.Value)
				}
			}
		}
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids})
	}
}

func (c *WipeCommand) deleteRoute53Zone(resourceType string) {
	res, err := c.r53conn.ListHostedZones(&route53.ListHostedZonesInput{})

	if err == nil {
		hzIds := []*string{}
		hzAttrs := []*map[string]string{}

		for _, hz := range res.HostedZones {
			if c.checkDelete(resourceType, hz.Id) {
				c.deleteRoute53Record(resourceType, hz.Id)
				hzIds = append(hzIds, hz.Id)
				hzAttrs = append(hzAttrs, &map[string]string{
					"force_destroy": "true",
					"name":          *hz.Name,
				})
			}
		}
		c.deleteResources(ResourceSet{Type: resourceType, Ids: hzIds, Attrs: hzAttrs})
	}
}

func (c *WipeCommand) deleteEfsFileSystem(resourceType string) {
	res, err := c.efsconn.DescribeFileSystems(&efs.DescribeFileSystemsInput{})

	if err == nil {
		fsIds := []*string{}
		mtIds := []*string{}

		for _, r := range res.FileSystems {
			if c.checkDelete(resourceType, r.Name) {
				res, err := c.efsconn.DescribeMountTargets(&efs.DescribeMountTargetsInput{
					FileSystemId: r.FileSystemId,
				})

				if err == nil {
					for _, r := range res.MountTargets {
						mtIds = append(mtIds, r.MountTargetId)
					}
				}

				fsIds = append(fsIds, r.FileSystemId)
			}
		}
		c.deleteResources(ResourceSet{Type: "aws_efs_mount_target", Ids: mtIds})
		c.deleteResources(ResourceSet{Type: resourceType, Ids: fsIds})
	}
}

func (c *WipeCommand) deleteInstances(resourceType string) {
	res, err := c.ec2conn.DescribeInstances(&ec2.DescribeInstancesInput{})

	if err == nil {
		ids := []*string{}
		tags := []*map[string]string{}

		for _, r := range res.Reservations {
			for _, in := range r.Instances {
				if *in.State.Name != "terminated" {
					m := &map[string]string{}
					for _, t := range in.Tags {
						(*m)[*t.Key] = *t.Value
					}

					if c.checkDelete(resourceType, in.InstanceId, m) {
						ids = append(ids, in.InstanceId)
						tags = append(tags, m)
					}
				}
			}
		}
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids, Tags: tags})
	}
}

func (c *WipeCommand) deleteIamUser(resourceType string) {
	res, err := c.iamconn.ListUsers(&iam.ListUsersInput{})

	if err == nil {
		ids := []*string{}
		pIds := []*string{}
		attrs := []*map[string]string{}
		pAttrs := []*map[string]string{}

		for _, u := range res.Users {
			if c.checkDelete(resourceType, u.UserName) {
				ups, err := c.iamconn.ListUserPolicies(&iam.ListUserPoliciesInput{
					UserName: u.UserName,
				})
				if err == nil {
					for _, up := range ups.PolicyNames {
						fmt.Println(*up)
					}
				}

				upols, err := c.iamconn.ListAttachedUserPolicies(&iam.ListAttachedUserPoliciesInput{
					UserName: u.UserName,
				})
				if err == nil {
					for _, upol := range upols.AttachedPolicies {
						pIds = append(pIds, upol.PolicyArn)
						pAttrs = append(pAttrs, &map[string]string{
							"user":       *u.UserName,
							"policy_arn": *upol.PolicyArn,
						})
					}
				}

				ids = append(ids, u.UserName)
				attrs = append(attrs, &map[string]string{
					"force_destroy": "true",
				})
			}
		}
		c.deleteResources(ResourceSet{Type: "aws_iam_user_policy_attachment", Ids: pIds, Attrs: pAttrs})
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids, Attrs: attrs})
	}
}

func (c *WipeCommand) deleteIamPolicy(resourceType string) {
	res, err := c.iamconn.ListPolicies(&iam.ListPoliciesInput{})

	//ps, err := c.iamconn.ListGroups(&iam.ListPoliciesInput{})

	if err == nil {
		ids := []*string{}
		eIds := []*string{}
		attributes := []*map[string]string{}

		for _, pol := range res.Policies {
			if c.checkDelete(resourceType, pol.Arn) {
				es, err := c.iamconn.ListEntitiesForPolicy(&iam.ListEntitiesForPolicyInput{
					PolicyArn: pol.Arn,
				})
				if err == nil {
					roles := []string{}
					users := []string{}
					groups := []string{}

					for _, u := range es.PolicyUsers {
						users = append(users, *u.UserName)
					}
					for _, g := range es.PolicyGroups {
						groups = append(groups, *g.GroupName)
					}
					for _, r := range es.PolicyRoles {
						roles = append(roles, *r.RoleName)
					}

					eIds = append(eIds, pol.Arn)
					attributes = append(attributes, &map[string]string{
						"policy_arn": *pol.Arn,
						"name":       *pol.PolicyName,
						"users":      strings.Join(users, "."),
						"roles":      strings.Join(roles, "."),
						"groups":     strings.Join(groups, "."),
					})
				}
				ids = append(ids, pol.Arn)
			}
		}
		c.deleteResources(ResourceSet{Type: "aws_iam_policy_attachment", Ids: eIds, Attrs: attributes})
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids})
	}
}

func (c *WipeCommand) deleteIamRole(resourceType string) {
	res, err := c.iamconn.ListRoles(&iam.ListRolesInput{})

	if err == nil {
		ids := []*string{}
		rpolIds := []*string{}
		rpolAttributes := []*map[string]string{}
		pIds := []*string{}

		for _, role := range res.Roles {
			if c.checkDelete(resourceType, role.RoleName) {
				rpols, err := c.iamconn.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
					RoleName: role.RoleName,
				})
				if err == nil {
					for _, rpol := range rpols.AttachedPolicies {
						rpolIds = append(rpolIds, rpol.PolicyArn)
						rpolAttributes = append(rpolAttributes, &map[string]string{
							"role":       *role.RoleName,
							"policy_arn": *rpol.PolicyArn,
						})
					}
				}

				rps, err := c.iamconn.ListRolePolicies(&iam.ListRolePoliciesInput{
					RoleName: role.RoleName,
				})
				if err == nil {
					for _, rp := range rps.PolicyNames {
						bla := *role.RoleName + ":" + *rp
						pIds = append(pIds, &bla)
					}
				}

				ips, err := c.iamconn.ListInstanceProfilesForRole(&iam.ListInstanceProfilesForRoleInput{
					RoleName: role.RoleName,
				})
				if err == nil {
					for _, ip := range ips.InstanceProfiles {
						fmt.Println(*ip.InstanceProfileName)
					}
				}
				ids = append(ids, role.RoleName)
			}
		}
		c.deleteResources(ResourceSet{Type: "aws_iam_role_policy_attachment", Ids: rpolIds, Attrs: rpolAttributes})
		c.deleteResources(ResourceSet{Type: "aws_iam_role_policy", Ids: pIds})
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids})
	}
}

func (c *WipeCommand) deleteKmsKeys(resourceType string) {
	res, err := c.kmsconn.ListKeys(&kms.ListKeysInput{})

	if err == nil {
		ids := []*string{}
		attributes := []*map[string]string{}

		for _, r := range res.Keys {
			req, res := c.kmsconn.DescribeKeyRequest(&kms.DescribeKeyInput{
				KeyId: r.KeyId,
			})
			err := req.Send();
			if err == nil {
				if *res.KeyMetadata.KeyState != "PendingDeletion" {
					attributes = append(attributes, &map[string]string{
						"key_id": *r.KeyId,
					})
					ids = append(ids, r.KeyArn)
				}
			}
		}
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids})
	}
}

func (c *WipeCommand) deleteAmis(resourceType string) {
	res, err := c.ec2conn.DescribeImages(&ec2.DescribeImagesInput{})

	if err == nil {
		ids := []*string{}
		tags := []*map[string]string{}
		//info := []string{}

		for _, r := range res.Images {
			m := &map[string]string{}
			for _, t := range r.Tags {
				(*m)[*t.Key] = *t.Value
			}

			if c.checkDelete(resourceType, r.ImageId, m) {
				// TODO filter name?
				ids = append(ids, r.ImageId)
				tags = append(tags, m)
				//info = append(info, toYaml(r))
			}
		}
		c.deleteResources(ResourceSet{Type: resourceType, Ids: ids, Tags: tags})
	}
}

func (c *WipeCommand) checkDelete(rType string, id *string, tags ...*map[string]string) bool {
	if rVal, ok := c.in[rType]; ok {
		if len(rVal.Ids) == 0 && len(rVal.Tags) == 0 {
			return true
		}
		for _, regex := range rVal.Ids {
			res, _ := regexp.MatchString(*regex, *id)
			if res {
				return true
			}
		}
		for k, v := range rVal.Tags {
			if len(tags) > 0 {
				t := tags[0]
				if tVal, ok := (*t)[k]; ok {
					res, _ := regexp.MatchString(v, tVal)
					if res {
						return true
					}
				}
			}
		}
	}
	return false
}

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func (c *WipeCommand) listStuff(resDelDesc ResourceDeleteDescription, r interface{}) {
	out, err := json.Marshal(r)
	if err != nil {
		panic(r)
	}

	data := map[string]interface{}{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.Decode(&data)
	jq := jsonq.NewQuery(data)
	result, err := jq.Array(resDelDesc.Name)

	if err == nil {
		ids := []*string{}
		tags := []*map[string]string{}

		for i := 0; i < len(result); i++ {
			id, err := jq.String(resDelDesc.Name, strconv.Itoa(i), resDelDesc.Id)
			if err == nil {
				tagMap, err := jq.Array(resDelDesc.Name, strconv.Itoa(i), "Tags")
				m := &map[string]string{}
				if err == nil {
					for j := 0; j < len(tagMap); j++ {
						key, keyErr := jq.String(resDelDesc.Name, strconv.Itoa(i), "Tags", strconv.Itoa(j), "Key")
						value, ValErr := jq.String(resDelDesc.Name, strconv.Itoa(i), "Tags", strconv.Itoa(j), "Value")
						if  keyErr == nil && ValErr == nil {
							(*m)[key] = value
						}
					}
				}
				if c.checkDelete(resDelDesc.TerraformResourceType, &id, m) {
					ids = append(ids, &id)
					tags = append(tags, m)
				}
			} else {
				fmt.Println(err)
			}
		}
		c.deleteResources(ResourceSet{Type: resDelDesc.TerraformResourceType, Ids: ids, Tags: tags})
	} else {
		fmt.Println(err)
	}

}

func toYaml(r interface{}) string {
	out, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(out))
	data := map[string]interface{}{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.Decode(&data)

	j2, err := _yaml2.JSONToYAML(out)
	if err != nil {
		fmt.Printf("err: %v\n", err)
	}
	return string(j2)
}
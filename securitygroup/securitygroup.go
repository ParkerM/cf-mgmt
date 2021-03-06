package securitygroup

import (
	"encoding/json"
	"fmt"
	"strings"

	cfclient "github.com/cloudfoundry-community/go-cfclient"
	"github.com/pivotalservices/cf-mgmt/config"
	"github.com/pivotalservices/cf-mgmt/space"
	"github.com/xchapter7x/lo"
)

//NewManager -
func NewManager(client CFClient, spaceMgr space.Manager, cfg config.Reader, peek bool) Manager {
	return &DefaultManager{
		Cfg:          cfg,
		Client:       client,
		SpaceManager: spaceMgr,
		Peek:         peek,
	}
}

//DefaultSecurityGroupManager -
type DefaultManager struct {
	Cfg          config.Reader
	SpaceManager space.Manager
	Client       CFClient
	Peek         bool
}

//CreateApplicationSecurityGroups -
func (m *DefaultManager) CreateApplicationSecurityGroups() error {
	spaceConfigs, err := m.Cfg.GetSpaceConfigs()
	if err != nil {
		return err
	}
	sgs, err := m.ListNonDefaultSecurityGroups()
	if err != nil {
		return err
	}

	for _, input := range spaceConfigs {
		space, err := m.SpaceManager.FindSpace(input.Org, input.Space)
		if err != nil {
			return err
		}

		// iterate through and assign named security groups to the space - ensuring that they are up to date is
		// done elsewhere.
		for _, securityGroupName := range input.ASGs {
			if sgInfo, ok := sgs[securityGroupName]; ok {
				err := m.AssignSecurityGroupToSpace(space, sgInfo)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("Security group [%s] does not exist", securityGroupName)
			}
		}

		if input.EnableSecurityGroup {
			sgName := fmt.Sprintf("%s-%s", input.Org, input.Space)
			var sgInfo cfclient.SecGroup
			var ok bool
			if sgInfo, ok = sgs[sgName]; ok {
				changed, err := m.hasSecurityGroupChanged(sgInfo, input.SecurityGroupContents)
				if err != nil {
					return err
				}
				if changed {
					if err := m.UpdateSecurityGroup(sgInfo, input.SecurityGroupContents); err != nil {
						return err
					}
				}
			} else {
				securityGroup, err := m.CreateSecurityGroup(sgName, input.SecurityGroupContents)
				if err != nil {
					return err
				}
				sgInfo = *securityGroup
			}
			err := m.AssignSecurityGroupToSpace(space, sgInfo)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *DefaultManager) ListSecurityGroups() (map[string]cfclient.SecGroup, error) {
	securityGroups := make(map[string]cfclient.SecGroup)
	secGroups, err := m.Client.ListSecGroups()
	if err != nil {
		return securityGroups, err
	}
	lo.G.Debug("Total security groups returned :", len(secGroups))
	for _, sg := range secGroups {
		securityGroups[sg.Name] = sg
	}
	return securityGroups, nil
}

//CreateGlobalSecurityGroups -
func (m *DefaultManager) CreateGlobalSecurityGroups() error {
	sgs, err := m.ListSecurityGroups()
	if err != nil {
		return err
	}
	securityGroupConfigs, err := m.Cfg.GetASGConfigs()
	if err != nil {
		return err
	}
	defaultSecurityGroupConfigs, err := m.Cfg.GetDefaultASGConfigs()
	if err != nil {
		return err
	}
	err = m.processSecurityGroups(securityGroupConfigs, sgs)
	if err != nil {
		return err
	}
	err = m.processSecurityGroups(defaultSecurityGroupConfigs, sgs)
	if err != nil {
		return err
	}

	return nil
}

//AssignDefaultSecurityGroups -
func (m *DefaultManager) AssignDefaultSecurityGroups() error {
	sgs, err := m.ListSecurityGroups()
	if err != nil {
		return err
	}
	globalConfig, err := m.Cfg.GetGlobalConfig()
	if err != nil {
		return err
	}

	for _, runningGroup := range globalConfig.RunningSecurityGroups {
		if group, ok := sgs[runningGroup]; ok {
			if !group.Running {
				err = m.AssignRunningSecurityGroup(group)
				if err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("Running security group [%s] does not exist", runningGroup)
		}
	}

	for _, stagingGroup := range globalConfig.StagingSecurityGroups {
		if group, ok := sgs[stagingGroup]; ok {
			if !group.Staging {
				err = m.AssignStagingSecurityGroup(group)
				if err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("Staging security group [%s] does not exist", stagingGroup)
		}
	}

	if globalConfig.EnableUnassignSecurityGroups {
		for groupName, group := range sgs {
			if group.Running && !m.contains(globalConfig.RunningSecurityGroups, groupName) {
				err = m.UnassignRunningSecurityGroup(group)
				if err != nil {
					return err
				}
			}
			if group.Staging && !m.contains(globalConfig.StagingSecurityGroups, groupName) {
				err = m.UnassignStagingSecurityGroup(group)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (m *DefaultManager) contains(list []string, groupName string) bool {
	groupNameToUpper := strings.ToUpper(groupName)
	for _, v := range list {
		if strings.ToUpper(v) == groupNameToUpper {
			return true
		}
	}
	return false
}

func (m *DefaultManager) processSecurityGroups(securityGroupConfigs []config.ASGConfig, sgs map[string]cfclient.SecGroup) error {
	for _, input := range securityGroupConfigs {
		sgName := input.Name

		// For every named security group
		// Check if it's a new group or Update
		if sgInfo, ok := sgs[sgName]; ok {
			changed, err := m.hasSecurityGroupChanged(sgInfo, input.Rules)
			if err != nil {
				return err
			}
			if changed {
				if err := m.UpdateSecurityGroup(sgInfo, input.Rules); err != nil {
					return err
				}
			}
		} else {
			if _, err := m.CreateSecurityGroup(sgName, input.Rules); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *DefaultManager) hasSecurityGroupChanged(sgInfo cfclient.SecGroup, rules string) (bool, error) {
	jsonBytes, err := json.Marshal(sgInfo.Rules)
	if err != nil {
		return false, err
	}
	secRules := []cfclient.SecGroupRule{}
	err = json.Unmarshal([]byte(rules), &secRules)
	if err != nil {
		return false, err
	}
	jsonBytesToCompare, err := json.Marshal(secRules)
	if err != nil {
		return false, err
	}
	match, err := DoesJsonMatch(string(jsonBytes), string(jsonBytesToCompare))
	if err != nil {
		return false, err
	}
	return !match, nil
}

func (m *DefaultManager) AssignSecurityGroupToSpace(space cfclient.Space, secGroup cfclient.SecGroup) error {
	for _, configuredSpace := range secGroup.SpacesData {
		if configuredSpace.Entity.Guid == space.Guid {
			return nil
		}
	}
	if m.Peek {
		lo.G.Infof("[dry-run]: assigning security group %s to space %s", secGroup.Name, space.Name)
		return nil
	}
	lo.G.Infof("assigning security group %s to space %s", secGroup.Name, space.Name)
	return m.Client.BindSecGroup(secGroup.Guid, space.Guid)
}

func (m *DefaultManager) CreateSecurityGroup(sgName, contents string) (*cfclient.SecGroup, error) {
	if m.Peek {
		lo.G.Infof("[dry-run]: creating securityGroup %s with contents %s", sgName, contents)
		return &cfclient.SecGroup{Name: "dry-run-name", Guid: "dry-run-guid"}, nil
	}
	securityGroupRules := []cfclient.SecGroupRule{}
	err := json.Unmarshal([]byte(contents), &securityGroupRules)
	if err != nil {
		return nil, err
	}
	lo.G.Infof("creating securityGroup %s with contents %s", sgName, contents)
	return m.Client.CreateSecGroup(sgName, securityGroupRules, nil)
}

func (m *DefaultManager) UpdateSecurityGroup(sg cfclient.SecGroup, contents string) error {
	if m.Peek {
		lo.G.Infof("[dry-run]: updating securityGroup %s with contents %s", sg.Name, contents)
		return nil
	}
	securityGroupRules := []cfclient.SecGroupRule{}
	err := json.Unmarshal([]byte(contents), &securityGroupRules)
	if err != nil {
		return err
	}
	lo.G.Infof("[dry-run]: updating securityGroup %s with contents %s", sg.Name, contents)
	_, err = m.Client.UpdateSecGroup(sg.Guid, sg.Name, securityGroupRules, nil)
	return err
}
func (m *DefaultManager) ListNonDefaultSecurityGroups() (map[string]cfclient.SecGroup, error) {
	securityGroups := make(map[string]cfclient.SecGroup)
	groupMap, err := m.ListSecurityGroups()
	if err != nil {
		return nil, err
	}
	for key, groupMap := range groupMap {
		if groupMap.Running == false && groupMap.Staging == false {
			securityGroups[key] = groupMap
		}
	}
	return securityGroups, nil
}

func (m *DefaultManager) ListDefaultSecurityGroups() (map[string]cfclient.SecGroup, error) {
	securityGroups := make(map[string]cfclient.SecGroup)
	groupMap, err := m.ListSecurityGroups()
	if err != nil {
		return nil, err
	}
	for key, groupMap := range groupMap {
		if groupMap.Running == true || groupMap.Staging == true {
			securityGroups[key] = groupMap
		}
	}
	return securityGroups, nil
}

func (m *DefaultManager) AssignRunningSecurityGroup(sg cfclient.SecGroup) error {
	if m.Peek {
		lo.G.Infof("[dry-run]: assigning sg %s as running security group", sg.Name)
		return nil
	}
	lo.G.Infof("assigning sg %s as running security group", sg.Name)
	return m.Client.BindRunningSecGroup(sg.Guid)
}
func (m *DefaultManager) AssignStagingSecurityGroup(sg cfclient.SecGroup) error {
	if m.Peek {
		lo.G.Infof("[dry-run]: assigning sg %s as staging security group", sg.Name)
		return nil
	}
	lo.G.Infof("assigning sg %s as staging security group", sg.Name)
	return m.Client.BindStagingSecGroup(sg.Guid)
}
func (m *DefaultManager) UnassignRunningSecurityGroup(sg cfclient.SecGroup) error {
	if m.Peek {
		lo.G.Infof("[dry-run]: unassinging sg %s as running security group", sg.Name)
		return nil
	}
	lo.G.Infof("unassinging sg %s as running security group", sg.Name)
	return m.Client.UnbindRunningSecGroup(sg.Guid)
}
func (m *DefaultManager) UnassignStagingSecurityGroup(sg cfclient.SecGroup) error {
	if m.Peek {
		lo.G.Infof("[dry-run]: unassigning sg %s as staging security group", sg.Name)
		return nil
	}
	lo.G.Infof("unassigning sg %s as staging security group", sg.Name)
	return m.Client.UnbindStagingSecGroup(sg.Guid)
}

func (m *DefaultManager) GetSecurityGroupRules(sgGUID string) ([]byte, error) {
	secGroup, err := m.Client.GetSecGroup(sgGUID)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(secGroup.Rules, "", "\t")
}

func (m *DefaultManager) ListSpaceSecurityGroups(spaceGUID string) (map[string]string, error) {
	secGroups, err := m.Client.ListSpaceSecGroups(spaceGUID)
	if err != nil {
		return nil, err
	}
	lo.G.Debug("Total security groups returned :", len(secGroups))
	names := make(map[string]string)
	for _, sg := range secGroups {
		if sg.Running == false && sg.Staging == false {
			names[sg.Name] = sg.Guid
		}
	}
	return names, nil
}

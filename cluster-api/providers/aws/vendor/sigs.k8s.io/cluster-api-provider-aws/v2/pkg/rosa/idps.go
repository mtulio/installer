package rosa

import (
	"fmt"
<<<<<<< HEAD

	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"github.com/openshift/rosa/pkg/ocm"
)

=======
	"net/http"

	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
)

// ListIdentityProviders retrieves the list of identity providers.
func (c *RosaClient) ListIdentityProviders(clusterID string) ([]*cmv1.IdentityProvider, error) {
	response, err := c.ocm.ClustersMgmt().V1().
		Clusters().Cluster(clusterID).
		IdentityProviders().
		List().Page(1).Size(-1).
		Send()
	if err != nil {
		return nil, handleErr(response.Error(), err)
	}

	return response.Items().Slice(), nil
}

// CreateIdentityProvider adds a new identity provider to the cluster.
func (c *RosaClient) CreateIdentityProvider(clusterID string, idp *cmv1.IdentityProvider) (*cmv1.IdentityProvider, error) {
	response, err := c.ocm.ClustersMgmt().V1().
		Clusters().Cluster(clusterID).
		IdentityProviders().
		Add().Body(idp).
		Send()
	if err != nil {
		return nil, handleErr(response.Error(), err)
	}
	return response.Body(), nil
}

// GetHTPasswdUserList retrieves the list of users of the provided _HTPasswd_ identity provider.
func (c *RosaClient) GetHTPasswdUserList(clusterID, htpasswdIDPId string) (*cmv1.HTPasswdUserList, error) {
	listResponse, err := c.ocm.ClustersMgmt().V1().Clusters().Cluster(clusterID).
		IdentityProviders().IdentityProvider(htpasswdIDPId).HtpasswdUsers().List().Send()
	if err != nil {
		if listResponse.Error().Status() == http.StatusNotFound {
			return nil, nil
		}
		return nil, handleErr(listResponse.Error(), err)
	}

	return listResponse.Items(), nil
}

// AddHTPasswdUser adds a new user to the provided _HTPasswd_ identity provider.
func (c *RosaClient) AddHTPasswdUser(username, password, clusterID, idpID string) error {
	htpasswdUser, _ := cmv1.NewHTPasswdUser().Username(username).Password(password).Build()
	response, err := c.ocm.ClustersMgmt().V1().Clusters().Cluster(clusterID).
		IdentityProviders().IdentityProvider(idpID).HtpasswdUsers().Add().Body(htpasswdUser).Send()
	if err != nil {
		return handleErr(response.Error(), err)
	}

	return nil
}

>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
const (
	clusterAdminUserGroup = "cluster-admins"
	clusterAdminIDPname   = "cluster-admin"
)

// CreateAdminUserIfNotExist creates a new admin user withe username/password in the cluster if username doesn't already exist.
// the user is granted admin privileges by being added to a special IDP called `cluster-admin` which will be created if it doesn't already exist.
<<<<<<< HEAD
func CreateAdminUserIfNotExist(client *ocm.Client, clusterID, username, password string) error {
	existingClusterAdminIDP, userList, err := findExistingClusterAdminIDP(client, clusterID)
=======
func (c *RosaClient) CreateAdminUserIfNotExist(clusterID, username, password string) error {
	existingClusterAdminIDP, userList, err := c.findExistingClusterAdminIDP(clusterID)
>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
	if err != nil {
		return fmt.Errorf("failed to find existing cluster admin IDP: %w", err)
	}
	if existingClusterAdminIDP != nil {
		if hasUser(username, userList) {
			// user already exist in the cluster
			return nil
		}
	}

	// Add admin user to the cluster-admins group:
<<<<<<< HEAD
	user, err := CreateUserIfNotExist(client, clusterID, clusterAdminUserGroup, username)
=======
	user, err := c.CreateUserIfNotExist(clusterID, clusterAdminUserGroup, username)
>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
	if err != nil {
		return fmt.Errorf("failed to add user '%s' to cluster '%s': %s",
			username, clusterID, err)
	}

	if existingClusterAdminIDP != nil {
		// add htpasswd user to existing idp
<<<<<<< HEAD
		err := client.AddHTPasswdUser(username, password, clusterID, existingClusterAdminIDP.ID())
=======
		err := c.AddHTPasswdUser(username, password, clusterID, existingClusterAdminIDP.ID())
>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
		if err != nil {
			return fmt.Errorf("failed to add htpassawoed user cluster-admin to existing idp: %s", existingClusterAdminIDP.ID())
		}

		return nil
	}

	// No ClusterAdmin IDP exists, create an Htpasswd IDP
	htpasswdIDP := cmv1.NewHTPasswdIdentityProvider().Users(cmv1.NewHTPasswdUserList().Items(
		cmv1.NewHTPasswdUser().Username(username).Password(password),
	))
	clusterAdminIDP, err := cmv1.NewIdentityProvider().
		Type(cmv1.IdentityProviderTypeHtpasswd).
		Name(clusterAdminIDPname).
		Htpasswd(htpasswdIDP).
		Build()
	if err != nil {
		return fmt.Errorf(
			"failed to create '%s' identity provider for cluster '%s'",
			clusterAdminIDPname,
			clusterID,
		)
	}

	// Add HTPasswd IDP to cluster
<<<<<<< HEAD
	_, err = client.CreateIdentityProvider(clusterID, clusterAdminIDP)
	if err != nil {
		// since we could not add the HTPasswd IDP to the cluster, roll back and remove the cluster admin
		if err := client.DeleteUser(clusterID, clusterAdminUserGroup, user.ID()); err != nil {
=======
	_, err = c.CreateIdentityProvider(clusterID, clusterAdminIDP)
	if err != nil {
		// since we could not add the HTPasswd IDP to the cluster, roll back and remove the cluster admin
		if err := c.DeleteUser(clusterID, clusterAdminUserGroup, user.ID()); err != nil {
>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
			return fmt.Errorf("failed to revert the admin user for cluster '%s': %w",
				clusterID, err)
		}
		return fmt.Errorf("failed to create identity cluster-admin idp: %w", err)
	}

	return nil
}

<<<<<<< HEAD
// CreateUserIfNotExist creates a new user with `username` and adds it to the group if it doesn't already exist.
func CreateUserIfNotExist(client *ocm.Client, clusterID string, group, username string) (*cmv1.User, error) {
	user, err := client.GetUser(clusterID, group, username)
	if user != nil || err != nil {
		return user, err
	}

	userCfg, err := cmv1.NewUser().ID(username).Build()
	if err != nil {
		return nil, fmt.Errorf("failed to create user '%s' for cluster '%s': %w", username, clusterID, err)
	}
	return client.CreateUser(clusterID, group, userCfg)
}

func findExistingClusterAdminIDP(client *ocm.Client, clusterID string) (
	htpasswdIDP *cmv1.IdentityProvider, userList *cmv1.HTPasswdUserList, reterr error) {
	idps, err := client.GetIdentityProviders(clusterID)
=======
func (c *RosaClient) findExistingClusterAdminIDP(clusterID string) (
	htpasswdIDP *cmv1.IdentityProvider, userList *cmv1.HTPasswdUserList, reterr error) {
	idps, err := c.ListIdentityProviders(clusterID)
>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
	if err != nil {
		reterr = fmt.Errorf("failed to get identity providers for cluster '%s': %v", clusterID, err)
		return
	}

	for _, idp := range idps {
		if idp.Name() == clusterAdminIDPname {
<<<<<<< HEAD
			itemUserList, err := client.GetHTPasswdUserList(clusterID, idp.ID())
=======
			itemUserList, err := c.GetHTPasswdUserList(clusterID, idp.ID())
>>>>>>> 9cb2dd3334 (cluster-api/providers/aws: vendor)
			if err != nil {
				reterr = fmt.Errorf("failed to get user list of the HTPasswd IDP of '%s: %s': %v", idp.Name(), clusterID, err)
				return
			}

			htpasswdIDP = idp
			userList = itemUserList
			return
		}
	}

	return
}

func hasUser(username string, userList *cmv1.HTPasswdUserList) bool {
	hasUser := false
	userList.Each(func(user *cmv1.HTPasswdUser) bool {
		if user.Username() == username {
			hasUser = true
			return false
		}
		return true
	})
	return hasUser
}

package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/canonical/go-dqlite/client"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/version"
	"github.com/pkg/errors"
)

// Bootstrap turns a non-clustered LXD instance into the first (and leader)
// node of a new LXD cluster.
//
// This instance must already have its cluster.https_address set and be listening
// on the associated network address.
func Bootstrap(state *state.State, gateway *Gateway, name string) error {
	// Check parameters
	if name == "" {
		return fmt.Errorf("node name must not be empty")
	}

	err := membershipCheckNoLeftoverClusterCert(state.OS.VarDir)
	if err != nil {
		return err
	}

	var address string

	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		// Fetch current network address and raft nodes
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed to fetch node configuration")
		}

		address = config.ClusterAddress()

		// Make sure node-local database state is in order.
		err = membershipCheckNodeStateForBootstrapOrJoin(tx, address)
		if err != nil {
			return err
		}

		// Add ourselves as first raft node
		err = tx.RaftNodeFirst(address)
		if err != nil {
			return errors.Wrap(err, "failed to insert first raft node")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Update our own entry in the nodes table.
	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Make sure cluster database state is in order.
		err := membershipCheckClusterStateForBootstrapOrJoin(tx)
		if err != nil {
			return err
		}

		// Add ourselves to the nodes table.
		err = tx.NodeUpdate(1, name, address)
		if err != nil {
			return errors.Wrap(err, "failed to update cluster node")
		}

		// Update our role list.
		err = tx.NodeAddRole(1, db.ClusterRoleDatabase)
		if err != nil {
			return errors.Wrapf(err, "Failed to add database role for the node")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Shutdown the gateway. This will trash any dqlite connection against
	// our in-memory dqlite driver and shutdown the associated raft
	// instance. We also lock regular access to the cluster database since
	// we don't want any other database code to run while we're
	// reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "failed to acquire cluster database lock")
	}

	err = gateway.Shutdown()
	if err != nil {
		return errors.Wrap(err, "failed to shutdown gRPC SQL gateway")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	err = gateway.init()
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}

	err = gateway.waitLeadership()
	if err != nil {
		return err
	}

	// The cluster certificates are symlinks against the regular node
	// certificate.
	for _, ext := range []string{".crt", ".key", ".ca"} {
		if ext == ".ca" && !shared.PathExists(filepath.Join(state.OS.VarDir, "server.ca")) {
			continue
		}
		err := os.Symlink("server"+ext, filepath.Join(state.OS.VarDir, "cluster"+ext))
		if err != nil {
			return errors.Wrap(err, "failed to create cluster cert symlink")
		}
	}

	// Make sure we can actually connect to the cluster database through
	// the network endpoint. This also releases the previously acquired
	// lock and makes the Go SQL pooling system invalidate the old
	// connection, so new queries will be executed over the new network
	// connection.
	err = state.Cluster.ExitExclusive(func(tx *db.ClusterTx) error {
		_, err := tx.Nodes()
		return err
	})
	if err != nil {
		return errors.Wrap(err, "cluster database initialization failed")
	}

	return nil
}

// Accept a new node and add it to the cluster.
//
// This instance must already be clustered.
//
// Return an updated list raft database nodes (possibly including the newly
// accepted node).
func Accept(state *state.State, gateway *Gateway, name, address string, schema, api, arch int) ([]db.RaftNode, error) {
	// Check parameters
	if name == "" {
		return nil, fmt.Errorf("node name must not be empty")
	}
	if address == "" {
		return nil, fmt.Errorf("node address must not be empty")
	}

	// Insert the new node into the nodes table.
	var id int64
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Check that the node can be accepted with these parameters.
		err := membershipCheckClusterStateForAccept(tx, name, address, schema, api)
		if err != nil {
			return err
		}

		// Add the new node
		id, err = tx.NodeAddWithArch(name, address, arch)
		if err != nil {
			return errors.Wrap(err, "Failed to insert new node into the database")
		}

		// Mark the node as pending, so it will be skipped when
		// performing heartbeats or sending cluster
		// notifications.
		err = tx.NodePending(id, true)
		if err != nil {
			return errors.Wrap(err, "Failed to mark the new node as pending")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Possibly insert the new node into the raft_nodes table (if we have
	// less than 3 database nodes).
	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get raft nodes from the log")
	}
	count := len(nodes) // Existing nodes
	voters := 0
	standbys := 0
	for _, node := range nodes {
		switch node.Role {
		case db.RaftVoter:
			voters++
		case db.RaftStandBy:
			standbys++
		}
	}
	node := db.RaftNode{ID: uint64(id), Address: address, Role: db.RaftSpare}
	if count > 1 && voters < MaxVoters {
		node.Role = db.RaftVoter
	} else if standbys < MaxStandBys {
		node.Role = db.RaftStandBy
	}
	nodes = append(nodes, node)

	return nodes, nil
}

// Join makes a non-clustered LXD node join an existing cluster.
//
// It's assumed that Accept() was previously called against the leader node,
// which handed the raft server ID.
//
// The cert parameter must contain the keypair/CA material of the cluster being
// joined.
func Join(state *state.State, gateway *Gateway, cert *shared.CertInfo, name string, raftNodes []db.RaftNode) error {
	// Check parameters
	if name == "" {
		return fmt.Errorf("node name must not be empty")
	}

	var address string
	err := state.Node.Transaction(func(tx *db.NodeTx) error {
		// Fetch current network address and raft nodes
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed to fetch node configuration")
		}
		address = config.ClusterAddress()

		// Make sure node-local database state is in order.
		err = membershipCheckNodeStateForBootstrapOrJoin(tx, address)
		if err != nil {
			return err
		}

		// Set the raft nodes list to the one that was returned by Accept().
		err = tx.RaftNodesReplace(raftNodes)
		if err != nil {
			return errors.Wrap(err, "failed to set raft nodes")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Get the local config keys for the cluster pools and networks. It
	// assumes that the local storage pools and networks match the cluster
	// networks, if not an error will be returned. Also get any outstanding
	// operation, typically there will be just one, created by the POST
	// /cluster/nodes request which triggered this code.
	var pools map[string]map[string]string
	var networks map[string]map[string]string
	var operations []db.Operation

	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		pools, err = tx.StoragePoolsNodeConfig()
		if err != nil {
			return err
		}
		networks, err = tx.NetworksNodeConfig()
		if err != nil {
			return err
		}
		operations, err = tx.Operations()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Lock regular access to the cluster database since we don't want any
	// other database code to run while we're reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "failed to acquire cluster database lock")
	}

	// Shutdown the gateway and wipe any raft data. This will trash any
	// gRPC SQL connection against our in-memory dqlite driver and shutdown
	// the associated raft instance.
	err = gateway.Shutdown()
	if err != nil {
		return errors.Wrap(err, "failed to shutdown gRPC SQL gateway")
	}
	err = os.RemoveAll(state.OS.GlobalDatabaseDir())
	if err != nil {
		return errors.Wrap(err, "failed to remove existing raft data")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	gateway.cert = cert
	err = gateway.init()
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}

	// If we are listed among the database nodes, join the raft cluster.
	var info *db.RaftNode
	for _, node := range raftNodes {
		if node.Address == address {
			info = &node
		}
	}
	if info == nil {
		panic("joining node not found")
	}
	logger.Info(
		"Joining dqlite raft cluster",
		log15.Ctx{"id": info.ID, "address": info.Address, "role": info.Role})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := client.FindLeader(
		ctx, gateway.NodeStore(),
		client.WithDialFunc(gateway.raftDial()),
		client.WithLogFunc(DqliteLog),
	)
	if err != nil {
		return errors.Wrap(err, "Failed to connect to cluster leader")
	}
	defer client.Close()
	err = client.Add(ctx, *info)
	if err != nil {
		return errors.Wrap(err, "Failed to join cluster")
	}

	// Make sure we can actually connect to the cluster database through
	// the network endpoint. This also releases the previously acquired
	// lock and makes the Go SQL pooling system invalidate the old
	// connection, so new queries will be executed over the new gRPC
	// network connection. Also, update the storage_pools and networks
	// tables with our local configuration.
	logger.Info("Migrate local data to cluster database")
	err = state.Cluster.ExitExclusive(func(tx *db.ClusterTx) error {
		node, err := tx.NodePendingByAddress(address)
		if err != nil {
			return errors.Wrap(err, "failed to get ID of joining node")
		}
		state.Cluster.NodeID(node.ID)
		tx.NodeID(node.ID)

		// Storage pools.
		ids, err := tx.StoragePoolIDsNotPending()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster storage pool IDs")
		}
		for name, id := range ids {
			err := tx.StoragePoolNodeJoin(id, node.ID)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's to the pool")
			}

			driver, err := tx.StoragePoolDriver(id)
			if err != nil {
				return errors.Wrap(err, "failed to get storage pool driver")
			}

			if shared.StringInSlice(driver, []string{"ceph", "cephfs"}) {
				// For ceph pools we have to create volume
				// entries for the joining node.
				err := tx.StoragePoolNodeJoinCeph(id, node.ID)
				if err != nil {
					return errors.Wrap(err, "failed to create ceph volumes for joining node")
				}
			} else {
				// For other pools we add the config provided by the joining node.
				config, ok := pools[name]
				if !ok {
					return fmt.Errorf("joining node has no config for pool %s", name)
				}
				err = tx.StoragePoolConfigAdd(id, node.ID, config)
				if err != nil {
					return errors.Wrap(err, "failed to add joining node's pool config")
				}
			}
		}

		// Networks.
		ids, err = tx.NetworkIDsNotPending()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster network IDs")
		}
		for name, id := range ids {
			config, ok := networks[name]
			if !ok {
				return fmt.Errorf("joining node has no config for network %s", name)
			}
			err := tx.NetworkNodeJoin(id, node.ID)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's to the network")
			}
			err = tx.NetworkConfigAdd(id, node.ID, config)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's network config")
			}
		}

		// Migrate outstanding operations.
		for _, operation := range operations {
			_, err := tx.OperationAdd("", operation.UUID, operation.Type)
			if err != nil {
				return errors.Wrapf(err, "failed to migrate operation %s", operation.UUID)
			}
		}

		// Remove the pending flag for ourselves
		// notifications.
		err = tx.NodePending(node.ID, false)
		if err != nil {
			return errors.Wrapf(err, "failed to unmark the node as pending")
		}

		// Update our role list if needed.
		if info.Role == db.RaftVoter {
			err = tx.NodeAddRole(node.ID, db.ClusterRoleDatabase)
			if err != nil {
				return errors.Wrapf(err, "Failed to add database role for the node")
			}
		}

		// Generate partial heartbeat request containing just a raft node list.
		notifyNodesUpdate(raftNodes, info.ID, cert)

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "cluster database initialization failed")
	}

	return nil
}

// Attempt to send a heartbeat to all other nodes to notify them of a new or
// changed member.
func notifyNodesUpdate(raftNodes []db.RaftNode, id uint64, cert *shared.CertInfo) {
	// Generate partial heartbeat request containing just a raft node list.
	hbState := &APIHeartbeat{}
	nodes := make([]db.NodeInfo, len(raftNodes))
	for i, raftNode := range raftNodes {
		nodes[i].ID = int64(raftNode.ID)
		nodes[i].Address = raftNode.Address
	}
	hbState.Update(false, raftNodes, nodes, 0)
	for _, node := range raftNodes {
		if node.ID == id {
			continue
		}
		go HeartbeatNode(context.Background(), node.Address, cert, hbState)
	}
}

// Rebalance the raft cluster, trying to see if we have a spare online node
// that we can promote to voter node if we are below membershipMaxRaftVoters,
// or to standby if we are below membershipMaxStandBys.
//
// If there's such spare node, return its address as well as the new list of
// raft nodes.
func Rebalance(state *state.State, gateway *Gateway) (string, []db.RaftNode, error) {
	// First get the current raft members, since this method should be
	// called after a node has left.
	currentRaftNodes, err := gateway.currentRaftNodes()
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to get current raft nodes")
	}

	// Fetch the nodes from the database, to get their last heartbeat
	// timestamp and check whether they are offline.
	nodesByAddress := map[string]db.NodeInfo{}
	var offlineThreshold time.Duration
	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed load cluster configuration")
		}
		offlineThreshold = config.OfflineThreshold()
		nodes, err := tx.Nodes()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster nodes")
		}
		for _, node := range nodes {
			nodesByAddress[node.Address] = node
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}

	// Group by role. If a node is offline, we'll try to demote it right
	// away.
	voters := make([]string, 0)
	standbys := make([]string, 0)
	candidates := make([]string, 0)
	for i, info := range currentRaftNodes {
		node := nodesByAddress[info.Address]
		if node.IsOffline(offlineThreshold) && info.Role != db.RaftSpare {
			// Even the heartbeat timestamp is not recent
			// enough, let's try to connect to the node,
			// just in case the heartbeat is lagging behind
			// for some reason and the node is actually up.
			client, err := Connect(node.Address, gateway.cert, true)
			if err == nil {
				_, _, err = client.GetServer()
			}
			if err != nil {
				client, err := gateway.getClient()
				if err != nil {
					return "", nil, errors.Wrap(err, "Failed to connect to local dqlite node")
				}
				defer client.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err = client.Assign(ctx, info.ID, db.RaftSpare)
				if err != nil {
					return "", nil, errors.Wrap(err, "Failed to demote offline node")
				}
				err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
					return tx.NodeRemoveRole(node.ID, db.ClusterRoleDatabase)
				})
				if err != nil {
					return "", nil, errors.Wrap(err, "Failed to update node role")
				}
				currentRaftNodes[i].Role = db.RaftSpare
				continue
			}
		}
		switch info.Role {
		case db.RaftVoter:
			voters = append(voters, info.Address)
		case db.RaftStandBy:
			standbys = append(standbys, info.Address)
		case db.RaftSpare:
			candidates = append(candidates, info.Address)
		}
	}

	var role db.RaftRole

	if len(voters) < MaxVoters && len(voters) > 1 {
		role = db.RaftVoter
		// Include stand-by nodes among the ones that can be promoted,
		// preferring them over spare ones.
		candidates = append(standbys, candidates...)
	} else if len(standbys) < MaxStandBys {
		role = db.RaftStandBy
	} else {
		// We're already at full capacity or would have a two-member cluster.
		return "", nil, nil
	}

	// Check if we have a spare node that we can promote to the missing role.
	address := ""
	for _, candidate := range candidates {
		node := nodesByAddress[candidate]
		logger.Debugf(
			"Found spare node %s (%s) to be promoted to %s", node.Name, node.Address, role)
		address = node.Address
		break
	}

	if address == "" {
		// No node to promote
		return "", currentRaftNodes, nil
	}

	for i, node := range currentRaftNodes {
		if node.Address == address {
			currentRaftNodes[i].Role = role
			break
		}
	}

	return address, currentRaftNodes, nil
}

// Assign a new role to the local dqlite node.
func Assign(state *state.State, gateway *Gateway, nodes []db.RaftNode) error {
	logger.Info("Assign new role to dqlite node")

	// Figure out our own address.
	address := ""
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		address, err = tx.NodeAddress()
		if err != nil {
			return errors.Wrap(err, "Failed to fetch the address of this cluster member")
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Sanity check that we actually have an address.
	if address == "" {
		return fmt.Errorf("Cluster member is not exposed on the network")
	}

	// Figure out our node identity.
	var info *db.RaftNode
	for i, node := range nodes {
		if node.Address == address {
			info = &nodes[i]
		}
	}

	// Sanity check that our address was actually included in the given
	// list of raft nodes.
	if info == nil {
		return fmt.Errorf("This node is not included in the given list of database nodes")
	}

	// Replace our local list of raft nodes with the given one (which
	// includes ourselves).
	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		err = tx.RaftNodesReplace(nodes)
		if err != nil {
			return errors.Wrap(err, "Failed to set raft nodes")
		}

		return nil
	})
	if err != nil {
		return err
	}

	var transactor func(func(tx *db.ClusterTx) error) error

	// If we are already running a dqlite node, it means we have cleanly
	// joined the cluster before, using the roles support API. In that case
	// there's no need to restart the gateway and we can just change our
	// dqlite role.
	if gateway.IsDqliteNode() {
		transactor = state.Cluster.Transaction
		goto assign
	}

	// If we get here it means that we are an upgraded node from cluster
	// without roles support, or we didn't cleanly join the cluster. Either
	// way, we don't have a dqlite node running, so we need to restart the
	// gateway.

	// Lock regular access to the cluster database since we don't want any
	// other database code to run while we're reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "Failed to acquire cluster database lock")
	}
	transactor = state.Cluster.ExitExclusive

	// Wipe all existing raft data, for good measure (perhaps they were
	// somehow leftover).
	err = os.RemoveAll(state.OS.GlobalDatabaseDir())
	if err != nil {
		return errors.Wrap(err, "Failed to remove existing raft data")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	err = gateway.init()
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}

assign:
	logger.Info(
		"Changing dqlite raft role",
		log15.Ctx{"id": info.ID, "address": info.Address, "role": info.Role})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := client.FindLeader(ctx, gateway.NodeStore(), client.WithDialFunc(gateway.raftDial()))
	if err != nil {
		return errors.Wrap(err, "Failed to connect to cluster leader")
	}
	defer client.Close()

	err = client.Assign(ctx, info.ID, info.Role)
	if err != nil {
		return errors.Wrap(err, "Failed to assign role")
	}

	gateway.info = info

	// Unlock regular access to our cluster database and add the database role.
	err = transactor(func(tx *db.ClusterTx) error {
		var f func(id int64, role db.ClusterRole) error
		if info.Role == db.RaftVoter {
			f = tx.NodeAddRole
		} else {
			f = tx.NodeRemoveRole
		}
		err = f(state.Cluster.GetNodeID(), db.ClusterRoleDatabase)
		if err != nil {
			return errors.Wrapf(err, "Failed to change role for the node")
		}
		return err
	})
	if err != nil {
		return errors.Wrap(err, "Cluster database initialization failed")
	}

	// Generate partial heartbeat request containing just a raft node list.
	notifyNodesUpdate(nodes, info.ID, gateway.cert)

	return nil
}

// Leave a cluster.
//
// If the force flag is true, the node will leave even if it still has
// containers and images.
//
// The node will only leave the raft cluster, and won't be removed from the
// database. That's done by Purge().
//
// Upon success, return the address of the leaving node.
//
// This function must be called by the cluster leader.
func Leave(state *state.State, gateway *Gateway, name string, force bool) (string, error) {
	logger.Debugf("Make node %s leave the cluster", name)

	// Check if the node can be deleted and track its address.
	var address string
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node (if it doesn't exists an error is returned).
		node, err := tx.NodeByName(name)
		if err != nil {
			return err
		}

		// Check that the node is eligeable for leaving.
		if !force {
			err := membershipCheckClusterStateForLeave(tx, node.ID)
			if err != nil {
				return err
			}
		}

		address = node.Address
		return nil
	})
	if err != nil {
		return "", err
	}

	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return "", err
	}
	var info *db.RaftNode // Raft node to remove, if any.
	for i, node := range nodes {
		if node.Address == address {
			info = &nodes[i]
			break
		}
	}

	if info == nil {
		// The node was not part of the raft cluster, nothing left to
		// do.
		return address, nil
	}

	// Get the address of another database node,
	logger.Info(
		"Remove node from dqlite raft cluster",
		log15.Ctx{"id": info.ID, "address": info.Address})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := gateway.getClient()
	if err != nil {
		return "", errors.Wrap(err, "Failed to connect to cluster leader")
	}
	defer client.Close()
	err = client.Remove(ctx, info.ID)
	if err != nil {
		return "", errors.Wrap(err, "Failed to leave the cluster")
	}

	return address, nil
}

// Handover looks for a non-voter member that can be promoted to replace a the
// member with the given address, which is shutting down. It returns the
// address of such member along with an updated list of nodes, with the ne role
// set.
//
// It should be called only by the current leader.
func Handover(state *state.State, gateway *Gateway, address string) (string, []db.RaftNode, error) {
	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return "", nil, errors.Wrap(err, "Failed to get current raft nodes")
	}

	// If the member which is shutting down is not a voter, there's nothing
	// to do.
	found := false
	for _, node := range nodes {
		if node.Address != address {
			continue
		}
		if node.Role != db.RaftVoter {
			return "", nil, nil
		}
		found = true
		break
	}
	if !found {
		return "", nil, errors.Wrapf(err, "No dqlite node has address %s", address)
	}

	for i, node := range nodes {
		if node.Role == db.RaftVoter || node.Address == address {
			continue
		}
		online, err := isMemberOnline(state, gateway.cert, node.Address)
		if err != nil {
			return "", nil, errors.Wrapf(err, "Failed to check if %s is online", node.Address)
		}
		if !online {
			continue
		}
		nodes[i].Role = db.RaftVoter
		return node.Address, nodes, nil
	}

	return "", nil, nil
}

// Check if the member with the given address is one.
func isMemberOnline(state *state.State, cert *shared.CertInfo, address string) (bool, error) {
	online := true
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		offlineThreshold, err := tx.NodeOfflineThreshold()
		if err != nil {
			return err
		}
		node, err := tx.NodeByAddress(address)
		if err != nil {
			return err
		}
		if node.IsOffline(offlineThreshold) {
			online = false
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	// Even if the heartbeat timestamp is not recent enough, let's try to
	// connect to the node, just in case the heartbeat is lagging behind
	// for some reason and the node is actually up.
	if !online && hasConnectivity(cert, address) {
		online = true
	}

	return online, nil
}

// Purge removes a node entirely from the cluster database.
func Purge(cluster *db.Cluster, name string) error {
	logger.Debugf("Remove node %s from the database", name)

	return cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node (if it doesn't exists an error is returned).
		node, err := tx.NodeByName(name)
		if err != nil {
			return errors.Wrapf(err, "failed to get node %s", name)
		}

		err = tx.NodeClear(node.ID)
		if err != nil {
			return errors.Wrapf(err, "failed to clear node %s", name)
		}

		err = tx.NodeRemove(node.ID)
		if err != nil {
			return errors.Wrapf(err, "failed to remove node %s", name)
		}
		return nil
	})
}

// List the nodes of the cluster.
func List(state *state.State) ([]api.ClusterMember, error) {
	var err error
	var nodes []db.NodeInfo
	var offlineThreshold time.Duration

	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		nodes, err = tx.Nodes()
		if err != nil {
			return err
		}

		offlineThreshold, err = tx.NodeOfflineThreshold()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	result := make([]api.ClusterMember, len(nodes))
	now := time.Now()
	version := nodes[0].Version()
	for i, node := range nodes {
		result[i].ServerName = node.Name
		result[i].URL = fmt.Sprintf("https://%s", node.Address)
		result[i].Database = shared.StringInSlice(string(db.ClusterRoleDatabase), node.Roles)
		result[i].Roles = node.Roles
		result[i].Architecture, err = osarch.ArchitectureName(node.Architecture)
		if err != nil {
			return nil, err
		}

		if node.IsOffline(offlineThreshold) {
			result[i].Status = "Offline"
			result[i].Message = fmt.Sprintf(
				"no heartbeat since %s", now.Sub(node.Heartbeat))
		} else {
			result[i].Status = "Online"
			result[i].Message = "fully operational"
		}

		n, err := util.CompareVersions(version, node.Version())
		if err != nil {
			result[i].Status = "Broken"
			result[i].Message = "inconsistent version"
			continue
		}

		if n == 1 {
			// This node's version is lower, which means the
			// version that the previous node in the loop has been
			// upgraded.
			version = node.Version()
		}
	}

	// Update the state of online nodes that have been upgraded and whose
	// schema is more recent than the rest of the nodes.
	for i, node := range nodes {
		if result[i].Status != "Online" {
			continue
		}
		n, err := util.CompareVersions(version, node.Version())
		if err != nil {
			continue
		}
		if n == 2 {
			result[i].Status = "Blocked"
			result[i].Message = "waiting for other nodes to be upgraded"
		}
	}

	return result, nil
}

// Count is a convenience for checking the current number of nodes in the
// cluster.
func Count(state *state.State) (int, error) {
	var count int
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		count, err = tx.NodesCount()
		return err
	})
	return count, err
}

// Enabled is a convenience that returns true if clustering is enabled on this
// node.
func Enabled(node *db.Node) (bool, error) {
	enabled := false
	err := node.Transaction(func(tx *db.NodeTx) error {
		addresses, err := tx.RaftNodeAddresses()
		if err != nil {
			return err
		}
		enabled = len(addresses) > 0
		return nil
	})
	return enabled, err
}

// Check that node-related preconditions are met for bootstrapping or joining a
// cluster.
func membershipCheckNodeStateForBootstrapOrJoin(tx *db.NodeTx, address string) error {
	nodes, err := tx.RaftNodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current raft nodes")
	}

	hasClusterAddress := address != ""
	hasRaftNodes := len(nodes) > 0

	// Sanity check that we're not in an inconsistent situation, where no
	// cluster address is set, but still there are entries in the
	// raft_nodes table.
	if !hasClusterAddress && hasRaftNodes {
		return fmt.Errorf("inconsistent state: found leftover entries in raft_nodes")
	}

	if !hasClusterAddress {
		return fmt.Errorf("no cluster.https_address config is set on this node")
	}
	if hasRaftNodes {
		return fmt.Errorf("the node is already part of a cluster")
	}

	return nil
}

// Check that cluster-related preconditions are met for bootstrapping or
// joining a cluster.
func membershipCheckClusterStateForBootstrapOrJoin(tx *db.ClusterTx) error {
	nodes, err := tx.Nodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current cluster nodes")
	}
	if len(nodes) != 1 {
		return fmt.Errorf("inconsistent state: found leftover entries in nodes")
	}
	return nil
}

// Check that cluster-related preconditions are met for accepting a new node.
func membershipCheckClusterStateForAccept(tx *db.ClusterTx, name string, address string, schema int, api int) error {
	nodes, err := tx.Nodes()
	if err != nil {
		return errors.Wrap(err, "Failed to fetch current cluster nodes")
	}

	if len(nodes) == 1 && nodes[0].Address == "0.0.0.0" {
		return fmt.Errorf("Clustering isn't enabled")
	}

	for _, node := range nodes {
		if node.Name == name {
			return fmt.Errorf("The cluster already has a member with name: %s", name)
		}

		if node.Address == address {
			return fmt.Errorf("The cluster already has a member with address: %s", address)
		}

		if node.Schema != schema {
			return fmt.Errorf("The joining server version doesn't (expected %s with DB schema %v)", version.Version, schema)
		}

		if node.APIExtensions != api {
			return fmt.Errorf("The joining server version doesn't (expected %s with API count %v)", version.Version, api)
		}
	}

	return nil
}

// Check that cluster-related preconditions are met for leaving a cluster.
func membershipCheckClusterStateForLeave(tx *db.ClusterTx, nodeID int64) error {
	// Check that it has no containers or images.
	message, err := tx.NodeIsEmpty(nodeID)
	if err != nil {
		return err
	}
	if message != "" {
		return fmt.Errorf(message)
	}

	// Check that it's not the last node.
	nodes, err := tx.Nodes()
	if err != nil {
		return err
	}
	if len(nodes) == 1 {
		return fmt.Errorf("node is the only node in the cluster")
	}
	return nil
}

// Check that there is no left-over cluster certificate in the LXD var dir of
// this node.
func membershipCheckNoLeftoverClusterCert(dir string) error {
	// Sanity check that there's no leftover cluster certificate
	for _, basename := range []string{"cluster.crt", "cluster.key", "cluster.ca"} {
		if shared.PathExists(filepath.Join(dir, basename)) {
			return fmt.Errorf("inconsistent state: found leftover cluster certificate")
		}
	}
	return nil
}

// SchemaVersion holds the version of the cluster database schema.
var SchemaVersion = cluster.SchemaVersion

// We currently aim at having 3 voter nodes and 2 stand-by.
//
// TODO: these numbers should probably be configurable.
const MaxVoters = 3
const MaxStandBys = 2

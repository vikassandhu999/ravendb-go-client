package ravendb

// DatabasePutResult describes server response for e.g. CreateDatabaseCommand
type DatabasePutResult struct {
	RaftCommandIndex int      `json:"RaftCommandIndex"`
	Name             string   `json:"Name"`
	DatabaseTopology Topology `json:"Topology"`
	NodesAddedTo     []string `json:"NodesAddedTo"`
}

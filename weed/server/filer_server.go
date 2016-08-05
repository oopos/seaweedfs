package weed_server

import (
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/filer/cassandra_store"
	"github.com/chrislusf/seaweedfs/weed/filer/embedded_filer"
	"github.com/chrislusf/seaweedfs/weed/filer/flat_namespace"
	"github.com/chrislusf/seaweedfs/weed/filer/redis_store"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/storage"
	"github.com/chrislusf/seaweedfs/weed/util"
)

type FilerServer struct {
	port               string
	master             string
	mnLock             sync.RWMutex
	collection         string
	defaultReplication string
	redirectOnRead     bool
	disableDirListing  bool
	secret             security.Secret
	filer              filer.Filer
	maxMB		   int
	masterNodes        *storage.MasterNodes
	get_guard          *security.Guard
	head_guard         *security.Guard
	delete_guard       *security.Guard
	put_guard          *security.Guard
	post_guard         *security.Guard
}

func NewFilerServer(r *http.ServeMux, ip string, port int, master string, dir string, collection string,
	replication string, redirectOnRead bool, disableDirListing bool,
	maxMB int,
	secret string,
	cassandra_server string, cassandra_keyspace string,
	redis_server string, redis_password string, redis_database int,
	get_ip_whitelist []string, head_ip_whitelist []string, delete_ip_whitelist []string, put_ip_whitelist []string, post_ip_whitelist []string,
	get_root_whitelist []string, head_root_whitelist []string, delete_root_whitelist []string, put_root_whitelist []string, post_root_whitelist []string,
	get_secure_key string, head_secure_key string, delete_secure_key string, put_secure_key string, post_secure_key string,
) (fs *FilerServer, err error) {
	fs = &FilerServer{
		master:             master,
		collection:         collection,
		defaultReplication: replication,
		redirectOnRead:     redirectOnRead,
		disableDirListing:  disableDirListing,
		maxMB:		    maxMB,
		get_guard:          security.NewGuard(get_ip_whitelist, get_root_whitelist, get_secure_key),
		head_guard:         security.NewGuard(head_ip_whitelist, head_root_whitelist, head_secure_key),
		delete_guard:       security.NewGuard(delete_ip_whitelist, delete_root_whitelist, delete_secure_key),
		put_guard:          security.NewGuard(put_ip_whitelist, put_root_whitelist, put_secure_key),
		post_guard:         security.NewGuard(post_ip_whitelist, post_root_whitelist, post_secure_key),
		port:               ip + ":" + strconv.Itoa(port),
	}

	if cassandra_server != "" {
		cassandra_store, err := cassandra_store.NewCassandraStore(cassandra_keyspace, cassandra_server)
		if err != nil {
			glog.Fatalf("Can not connect to cassandra server %s with keyspace %s: %v", cassandra_server, cassandra_keyspace, err)
		}
		fs.filer = flat_namespace.NewFlatNamespaceFiler(master, cassandra_store)
	} else if redis_server != "" {
		redis_store := redis_store.NewRedisStore(redis_server, redis_password, redis_database)
		fs.filer = flat_namespace.NewFlatNamespaceFiler(master, redis_store)
	} else {
		if fs.filer, err = embedded_filer.NewFilerEmbedded(master, dir); err != nil {
			glog.Fatalf("Can not start filer in dir %s : %v", dir, err)
			return
		}

		r.HandleFunc("/admin/mv", fs.moveHandler)
		r.HandleFunc("/admin/register", fs.registerHandler)
	}

	r.HandleFunc("/", fs.filerHandler)

	go func() {
		connected := true

		fs.masterNodes = storage.NewMasterNodes(fs.master)
		glog.V(0).Infof("Filer server bootstraps with master %s", fs.getMasterNode())

		//force initialize with all available master nodes
		for {
			_, err := fs.masterNodes.FindMaster()
			if err != nil {
				glog.Infof("filer server failed to get master cluster info:%s", err.Error())
				time.Sleep(3 * time.Second)
			} else {
				break
			}
		}

		for {
			glog.V(4).Infof("Filer server sending to master %s", fs.getMasterNode())
			master, err := fs.detectHealthyMaster(fs.getMasterNode())
			if err == nil {
				if !connected {
					connected = true
					if fs.getMasterNode() != master {
						fs.setMasterNode(master)
					}
					glog.V(0).Infoln("Filer Server Connected with master at", master)
				}
			} else {
				glog.V(1).Infof("Filer Server Failed to talk with master %s: %v", fs.getMasterNode(), err)
				if connected {
					connected = false
				}
			}
			if connected {
				time.Sleep(time.Duration(float32(10*1e3)*(1+rand.Float32())) * time.Millisecond)
			} else {
				time.Sleep(time.Duration(float32(10*1e3)*0.25) * time.Millisecond)
			}
		}
	}()

	return fs, nil
}

func (fs *FilerServer) jwt(fileId string) security.EncodedJwt {
	return security.GenJwt(fs.secret, fileId)
}

func (fs *FilerServer) getMasterNode() string {
	fs.mnLock.RLock()
	defer fs.mnLock.RUnlock()
	return fs.master
}

func (fs *FilerServer) setMasterNode(masterNode string) {
	fs.mnLock.Lock()
	defer fs.mnLock.Unlock()
	fs.master = masterNode
}

func (fs *FilerServer) detectHealthyMaster(masterNode string) (master string, e error) {
	if e = checkMaster(masterNode); e != nil {
		fs.masterNodes.Reset()
		for i := 0; i <= 3; i++ {
			master, e = fs.masterNodes.FindMaster()
			if e != nil {
				continue
			} else {
				if e = checkMaster(master); e == nil {
					break
				}
			}
		}
	} else {
		master = masterNode
	}
	return
}

func checkMaster(masterNode string) error {
	statUrl := "http://" + masterNode + "/stats"
	glog.V(4).Infof("Connecting to %s ...", statUrl)
	_, e := util.Get(statUrl)
	return e
}

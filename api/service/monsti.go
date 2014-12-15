// This file is part of Monsti, a web content management system.
// Copyright 2012-2014 Christian Neumann
//
// Monsti is free software: you can redistribute it and/or modify it under the
// terms of the GNU Affero General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option) any
// later version.
//
// Monsti is distributed in the hope that it will be useful, but WITHOUT ANY
// WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR
// A PARTICULAR PURPOSE.  See the GNU Affero General Public License for more
// details.
//
// You should have received a copy of the GNU Affero General Public License
// along with Monsti.  If not, see <http://www.gnu.org/licenses/>.

package service

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/chrneumann/mimemail"
)

// MonstiClient represents the RPC connection to the Monsti service.
type MonstiClient struct {
	Client
	SignalHandlers map[string]func(interface{}) (interface{}, error)
}

// NewMonstiConnection establishes a new RPC connection to a Monsti service.
//
// path is the unix domain socket path to the service.
func NewMonstiConnection(path string) (*MonstiClient, error) {
	var service MonstiClient
	if err := service.Connect(path); err != nil {
		return nil,
			fmt.Errorf("service: Could not establish connection to Monsti service: %v",
				err)
	}
	return &service, nil
}

// ModuleInitDone tells Monsti that the given module has finished its
// initialization. Monsti won't finish its startup until all modules
// called this method.
func (s *MonstiClient) ModuleInitDone(module string) error {
	if s.Error != nil {
		return s.Error
	}
	err := s.RPCClient.Call("Monsti.ModuleInitDone", module, new(int))
	if err != nil {
		return fmt.Errorf("service: ModuleInitDone error: %v", err)
	}
	return nil
}

// nodeToData converts the node to a JSON document.
// The Path field will be omitted.
func nodeToData(node *Node, indent bool) ([]byte, error) {
	var data []byte
	var err error
	path := node.Path
	node.Path = ""
	defer func() {
		node.Path = path
	}()

	var outNode nodeJSON
	outNode.Node = *node
	outNode.Type = node.Type.Id
	outNode.Fields = make(map[string]map[string]*json.RawMessage)

	nodeFields := append(node.Type.Fields, node.LocalFields...)
	for _, field := range nodeFields {
		parts := strings.SplitN(field.Id, ".", 2)
		dump, err := json.Marshal(node.Fields[field.Id].Dump())
		if err != nil {
			return nil, fmt.Errorf("Could not marshal field: %v", err)
		}
		if outNode.Fields[parts[0]] == nil {
			outNode.Fields[parts[0]] = make(map[string]*json.RawMessage)
		}
		msg := json.RawMessage(dump)
		outNode.Fields[parts[0]][parts[1]] = &msg
	}

	if indent {
		data, err = json.MarshalIndent(outNode, "", "  ")
	} else {
		data, err = json.Marshal(outNode)
	}
	if err != nil {
		return nil, fmt.Errorf(
			"service: Could not marshal node: %v", err)
	}
	return data, nil
}

// WriteNode writes the given node.
func (s *MonstiClient) WriteNode(site, path string, node *Node) error {
	if s.Error != nil {
		return nil
	}
	node.Changed = time.Now().UTC()
	data, err := nodeToData(node, true)
	if err != nil {
		return fmt.Errorf("service: Could not convert node: %v", err)
	}
	err = s.WriteNodeData(site, path, "node.json", data)
	if err != nil {
		return fmt.Errorf(
			"service: Could not write node: %v", err)
	}
	return nil
}

type nodeJSON struct {
	Node
	Type   string
	Fields map[string]map[string]*json.RawMessage
}

// dataToNode unmarshals given data
func dataToNode(data []byte,
	getNodeType func(id string) (*NodeType, error), m *MonstiClient, site string) (
	*Node, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var node nodeJSON
	err := json.Unmarshal(data, &node)
	if err != nil {
		return nil, fmt.Errorf(
			"service: Could not unmarshal node: %v", err)
	}
	ret := node.Node
	ret.Type, err = getNodeType(node.Type)
	if err != nil {
		return nil, fmt.Errorf("Could not get node type %q: %v",
			node.Type, err)
	}

	if err = ret.InitFields(m, site); err != nil {
		return nil, fmt.Errorf("Could not init node fields: %v", err)
	}
	nodeFields := append(ret.Type.Fields, ret.LocalFields...)
	for _, field := range nodeFields {
		parts := strings.SplitN(field.Id, ".", 2)
		value := node.Fields[parts[0]][parts[1]]
		if value != nil {
			f := func(in interface{}) error {
				return json.Unmarshal(*value, in)
			}
			ret.Fields[field.Id].Load(f)
		}
	}
	return &ret, nil
}

// GetNode reads the given node.
//
// If the node does not exist, it returns nil, nil.
func (s *MonstiClient) GetNode(site, path string) (*Node, error) {
	if s.Error != nil {
		return nil, nil
	}
	args := struct{ Site, Path string }{site, path}
	var reply []byte
	err := s.RPCClient.Call("Monsti.GetNode", args, &reply)
	if err != nil {
		return nil, fmt.Errorf("service: GetNode error: %v", err)
	}
	node, err := dataToNode(reply, s.GetNodeType, s, site)
	if err != nil {
		return nil, fmt.Errorf("service: Could not convert node: %v", err)
	}
	return node, nil
}

// GetChildren returns the children of the given node.
func (s *MonstiClient) GetChildren(site, path string) ([]*Node, error) {
	if s.Error != nil {
		return nil, s.Error
	}
	args := struct{ Site, Path string }{site, path}
	var reply [][]byte
	err := s.RPCClient.Call("Monsti.GetChildren", args, &reply)
	if err != nil {
		return nil, fmt.Errorf("service: GetChildren error: %v", err)
	}
	nodes := make([]*Node, 0, len(reply))
	for _, entry := range reply {

		node, err := dataToNode(entry, s.GetNodeType, s, site)
		if err != nil {
			return nil, fmt.Errorf("service: Could not convert node: %v", err)
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// GetNodeData requests data from some node.
//
// Returns a nil slice and nil error if the data does not exist.
func (s *MonstiClient) GetNodeData(site, path, file string) ([]byte, error) {
	if s.Error != nil {
		return nil, s.Error
	}
	type GetNodeDataArgs struct {
	}
	args := struct{ Site, Path, File string }{
		site, path, file}
	var reply []byte
	err := s.RPCClient.Call("Monsti.GetNodeData", &args, &reply)
	if err != nil {
		return nil, fmt.Errorf("service: GetNodeData error:", err)
	}
	return reply, nil
}

// WriteNodeData writes data for some node.
func (s *MonstiClient) WriteNodeData(site, path, file string,
	content []byte) error {
	if s.Error != nil {
		return nil
	}
	args := struct {
		Site, Path, File string
		Content          []byte
	}{
		site, path, file, content}
	if err := s.RPCClient.Call("Monsti.WriteNodeData", &args, new(int)); err != nil {
		return fmt.Errorf("service: WriteNodeData error: %v", err)
	}
	return nil
}

// RemoveNode recursively removes the given site's node.
func (s *MonstiClient) RemoveNode(site string, node string) error {
	if s.Error != nil {
		return nil
	}
	args := struct {
		Site, Node string
	}{site, node}
	if err := s.RPCClient.Call("Monsti.RemoveNode", args, new(int)); err != nil {
		return fmt.Errorf("service: RemoveNode error: %v", err)
	}
	return nil
}

// RenameNode renames (moves) the given site's node.
//
// Source and target path must be absolute
func (s *MonstiClient) RenameNode(site, source, target string) error {
	if s.Error != nil {
		return nil
	}
	args := struct {
		Site, Source, Target string
	}{site, source, target}
	if err := s.RPCClient.Call("Monsti.RenameNode", args, new(int)); err != nil {
		return fmt.Errorf("service: RenameNode error: %v", err)
	}
	return nil
}

func getConfig(reply []byte, out interface{}) error {
	if len(reply) == 0 {
		return nil
	}
	objectV := reflect.New(
		reflect.MapOf(reflect.TypeOf(""), reflect.TypeOf(out)))
	err := json.Unmarshal(reply, objectV.Interface())
	if err != nil {
		return fmt.Errorf("service: Could not decode configuration: %v", err)
	}
	value := objectV.Elem().MapIndex(
		objectV.Elem().MapKeys()[0])
	if !value.IsNil() {
		reflect.ValueOf(out).Elem().Set(value.Elem())
	}
	return nil
}

// GetSiteConfig puts the named site local configuration into the
// variable out.
func (s *MonstiClient) GetSiteConfig(site, name string, out interface{}) error {
	if s.Error != nil {
		return s.Error
	}
	args := struct{ Site, Name string }{site, name}
	var reply []byte
	err := s.RPCClient.Call("Monsti.GetSiteConfig", args, &reply)
	if err != nil {
		return fmt.Errorf("service: GetSiteConfig error: %v", err)
	}
	return getConfig(reply, out)
}

/*

// GetConfig puts the named global configuration into the variable out.
func (s *MonstiClient) GetConfig(name string, out interface{}) error {
	if s.Error != nil {
		return s.Error
	}
	var reply []byte
	err := s.RPCClient.Call("Monsti.GetConfig", name, &reply)
	if err != nil {
		return fmt.Errorf("service: GetConfig error: %v", err)
	}
	return getConfig(reply, out)
}

*/

// RegisterNodeType registers a new node type.
//
// Known field types will be reused. Just specify the id. All other //
// attributes of the field type will be ignored in this case.
func (s *MonstiClient) RegisterNodeType(nodeType *NodeType) error {
	if s.Error != nil {
		return s.Error
	}
	err := s.RPCClient.Call("Monsti.RegisterNodeType", nodeType, new(int))
	if err != nil {
		return fmt.Errorf("service: Error calling RegisterNodeType: %v", err)
	}
	return nil
}

// GetNodeType requests information about the given node type.
func (s *MonstiClient) GetNodeType(nodeTypeID string) (*NodeType,
	error) {
	if s.Error != nil {
		return nil, s.Error
	}
	var nodeType NodeType
	err := s.RPCClient.Call("Monsti.GetNodeType", nodeTypeID, &nodeType)
	if err != nil {
		return nil, fmt.Errorf("service: Error calling GetNodeType: %v", err)
	}
	return &nodeType, nil
}

// GetAddableNodeTypes returns the node types that may be added as child nodes
// to the given node type at the given website.
func (s *MonstiClient) GetAddableNodeTypes(site, nodeType string) (types []string,
	err error) {
	if s.Error != nil {
		return nil, s.Error
	}
	args := struct{ Site, NodeType string }{site, nodeType}
	err = s.RPCClient.Call("Monsti.GetAddableNodeTypes", args, &types)
	if err != nil {
		err = fmt.Errorf("service: Error calling GetAddableNodeTypes: %v", err)
	}
	return
}

/*

// RequestFile stores the path or content of a multipart request's file.
type RequestFile struct {
	// TmpFile stores the path to a temporary file with the contents.
	TmpFile string
	// Content stores the file content if TmpFile is not set.
	Content []byte
}

// ReadFile returns the file's content. Uses io/ioutil ReadFile if the request
// file's content is in a temporary file.
func (r RequestFile) ReadFile() ([]byte, error) {
	if len(r.TmpFile) > 0 {
		return ioutil.ReadFile(r.TmpFile)
	}
	return r.Content, nil
}
*/

type RequestMethod uint

const (
	GetRequest = iota
	PostRequest
)

type Action uint

const (
	ViewAction = iota
	EditAction
	LoginAction
	LogoutAction
	AddAction
	RemoveAction
	RequestPasswordTokenAction
	ChangePasswordAction
)

// A request to be processed by a nodes service.
type Request struct {
	Id       uint
	NodePath string
	// Site name
	Site string
	// The query values of the request URL.
	Query url.Values
	// Method of the request (GET,POST,...).
	Method RequestMethod
	// User session
	Session *UserSession
	// Action to perform (e.g. "edit").
	Action Action
	// FormData stores the requests form data.
	FormData url.Values
	/*
			// The requested node.
			Node *Node
		// Files stores files of multipart requests.
				Files map[string][]RequestFile
	*/
}

// GetRequest returns the request with the given id.
//
// If there is no request with the given id, it returns nil.
func (s *MonstiClient) GetRequest(id uint) (*Request, error) {
	if s.Error != nil {
		return nil, s.Error
	}
	var req Request
	if err := s.RPCClient.Call("Monsti.GetRequest", id, &req); err != nil {
		return nil, fmt.Errorf("service: Monsti.GetRequest error: %v", err)
	}
	if req.Id != id {
		return nil, nil
	}
	return &req, nil
}

/*
// Response to a node request.
type Response struct {
	// The html content to be embedded in the root template.
	Body []byte
	// Raw must be set to true if Body should not be embedded in the root
	// template. The content type will be automatically detected.
	Raw bool
	// If set, redirect to this target using error 303 'see other'.
	Redirect string
	// The node as received by GetRequest, possibly with some fields
	// updated (e.g. modified title).
	//
	// If nil, the original node data is used.
	Node *Node
}
*/

/*
// Write appends the given bytes to the body of the response.
func (r *Response) Write(p []byte) (n int, err error) {
	r.Body = append(r.Body, p...)
	return len(p), nil
}
*/

/*
// Request performs the given request.
func (s *MonstiClient) Request(req *Request) (*Response, error) {
	var res Response
	err := s.RPCClient.Call("Monsti.Request", req, &res)
	if err != nil {
		return nil, fmt.Errorf("service: RPC error for Request: %v", err)
	}
	return &res, nil
}
*/

// GetNodeType returns all supported node types.
func (s *MonstiClient) GetNodeTypes() ([]string, error) {
	if s.Error != nil {
		return nil, s.Error
	}
	var res []string
	err := s.RPCClient.Call("Monsti.GetNodeTypes", 0, &res)
	if err != nil {
		return nil, fmt.Errorf("service: RPC error for GetNodeTypes: %v", err)
	}
	return res, nil
}

// PublishService informs the INFO service about a new service.
//
// service is the identifier of the service
// path is the path to the unix domain socket of the service
//
// If the data does not exist, return null length []byte.
func (s *MonstiClient) PublishService(service, path string) error {
	args := struct{ Service, Path string }{service, path}
	if s.Error != nil {
		return s.Error
	}
	var reply int
	err := s.RPCClient.Call("Monsti.PublishService", args, &reply)
	if err != nil {
		return fmt.Errorf("service: Error calling PublishService: %v", err)
	}
	return nil
}

/*
// FindDataService requests a data client.
func (s *MonstiClient) FindDataService() (*MonstiClient, error) {
	var path string
	err := s.RPCClient.Call("Monsti.FindDataService", 0, &path)
	if err != nil {
		return nil, fmt.Errorf("service: Error calling FindDataService: %v", err)
	}
	service_ := NewDataClient()
	if err := service_.Connect(path); err != nil {
		return nil,
			fmt.Errorf("service: Could not establish connection to data service: %v",
				err)
	}
	return service_, nil
}
*/

// User represents a registered user of the site.
type User struct {
	Login string
	Name  string
	Email string
	// Hashed password.
	Password string
	// PasswordChanged keeps the time of the last password change.
	PasswordChanged time.Time
}

// UserSession is a session of an authenticated or anonymous user.
type UserSession struct {
	// Authenticaded user or nil
	User *User
	// Locale used for this session.
	Locale string
}

// Send given Monsti.
func (s *MonstiClient) SendMail(m *mimemail.Mail) error {
	if s.Error != nil {
		return s.Error
	}
	var reply int
	if err := s.RPCClient.Call("Monsti.SendMail", m, &reply); err != nil {
		return fmt.Errorf("service: Monsti.SendMail error: %v", err)
	}
	return nil
}

// AddSignalHandler connects to a signal with the given signal handler.
//
// Currently, you can only set one handler per signal and MonstiClient.
//
// Be sure to wait for incoming signals by calling WaitSignal() on
// this MonstiClient!
func (s *MonstiClient) AddSignalHandler(handler SignalHandler) error {
	if s.Error != nil {
		return s.Error
	}
	args := struct{ Id, Signal string }{s.Id, handler.Name()}
	err := s.RPCClient.Call("Monsti.ConnectSignal", args, new(int))
	if err != nil {
		return fmt.Errorf("service: Monsti.ConnectSignal error: %v", err)
	}
	if s.SignalHandlers == nil {
		s.SignalHandlers = make(map[string]func(interface{}) (interface{}, error))
	}
	s.SignalHandlers[handler.Name()] = handler.Handle
	return nil
}

type argWrap struct{ Wrap interface{} }

// EmitSignal emits the named signal with given arguments and return
// value.
func (s *MonstiClient) EmitSignal(name string, args interface{},
	retarg interface{}) error {
	if s.Error != nil {
		return s.Error
	}
	gob.RegisterName(name+"Ret", reflect.Zero(
		reflect.TypeOf(retarg).Elem().Elem()).Interface())
	gob.RegisterName(name+"Args", args)
	var args_ struct {
		Name string
		Args []byte
	}
	buffer := &bytes.Buffer{}
	enc := gob.NewEncoder(buffer)
	err := enc.Encode(argWrap{args})
	if err != nil {
		return fmt.Errorf("service: Could not encode signal argumens: %v", err)
	}
	args_.Name = name
	args_.Args = buffer.Bytes()
	var ret [][]byte
	err = s.RPCClient.Call("Monsti.EmitSignal", args_, &ret)
	if err != nil {
		return fmt.Errorf("service: Monsti.EmitSignal error: %v", err)
	}
	reflect.ValueOf(retarg).Elem().Set(reflect.MakeSlice(
		reflect.TypeOf(retarg).Elem(), len(ret), len(ret)))
	for i, answer := range ret {
		buffer = bytes.NewBuffer(answer)
		dec := gob.NewDecoder(buffer)
		var ret_ argWrap
		err = dec.Decode(&ret_)
		if err != nil {
			return fmt.Errorf("service: Could not decode signal return value: %v", err)
		}
		reflect.ValueOf(retarg).Elem().Index(i).Set(reflect.ValueOf(ret_.Wrap))
	}
	return nil
}

// WaitSignal waits for the next emitted signal.
//
// You have to connect to some signals before. See AddSignalHandler.
// This method must not be called in parallel by the same client
// instance.
func (s *MonstiClient) WaitSignal() error {
	if s.Error != nil {
		return s.Error
	}
	signal := struct {
		Name string
		Args []byte
	}{}
	err := s.RPCClient.Call("Monsti.WaitSignal", s.Id, &signal)
	if err != nil {
		return fmt.Errorf("service: Monsti.WaitSignal error: %v", err)
	}
	buffer := bytes.NewBuffer(signal.Args)
	dec := gob.NewDecoder(buffer)
	var args_ argWrap
	err = dec.Decode(&args_)
	if err != nil {
		return fmt.Errorf("service: Could not decode signal argumens: %v", err)
	}
	ret, err := s.SignalHandlers[signal.Name](args_.Wrap)
	if err != nil {
		return fmt.Errorf("service: Signal handler for %v returned error: %v",
			signal.Name, err)
	}
	signalRet := &struct {
		Id  string
		Ret []byte
	}{Id: s.Id}
	buffer = &bytes.Buffer{}
	enc := gob.NewEncoder(buffer)
	err = enc.Encode(argWrap{ret})
	if err != nil {
		return fmt.Errorf("service: Could not encode signal return value: %v", err)
	}
	signalRet.Ret = buffer.Bytes()
	err = s.RPCClient.Call("Monsti.FinishSignal", signalRet, new(int))
	if err != nil {
		return fmt.Errorf("service: Monsti.FinishSignal error: %v", err)
	}
	return nil
}

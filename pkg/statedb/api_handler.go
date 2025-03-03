// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package statedb

import (
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"net/http"

	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware"

	. "github.com/cilium/cilium/api/v1/server/restapi/statedb"
	restapi "github.com/cilium/cilium/api/v1/server/restapi/statedb"
	"github.com/cilium/cilium/pkg/api"
)

func newDumpHandler(db *DB) restapi.GetStatedbDumpHandler {
	return &dumpHandler{db}
}

// REST API handler for the '/statedb/dump' to dump the contents of the database
// as JSON. Available through `cilium statedb dump` and included in sysdumps.
type dumpHandler struct {
	db *DB
}

func (h *dumpHandler) Handle(params GetStatedbDumpParams) middleware.Responder {
	return middleware.ResponderFunc(func(w http.ResponseWriter, _ runtime.Producer) {
		h.db.ReadTxn().WriteJSON(w)
	})
}

// REST API handler for '/statedb/query' to perform remote Get() and LowerBound()
// queries against the database from 'cilium-dbg'.
func newQueryHandler(db *DB) restapi.GetStatedbQueryTableHandler {
	return &queryHandler{db}
}

type queryHandler struct {
	db *DB
}

// /statedb/query
func (h *queryHandler) Handle(params GetStatedbQueryTableParams) middleware.Responder {
	key, err := base64.StdEncoding.DecodeString(params.Key)
	if err != nil {
		return api.Error(GetStatedbQueryTableBadRequestCode, fmt.Errorf("Invalid key: %w", err))
	}

	txn := h.db.ReadTxn()
	indexTxn, err := txn.getTxn().indexReadTxn(params.Table, params.Index)
	if err != nil {
		return api.Error(GetStatedbQueryTableNotFoundCode, err)
	}

	iter := indexTxn.Root().Iterator()
	if params.Lowerbound {
		iter.SeekLowerBound(key)
	} else {
		iter.SeekPrefixWatch(key)
	}

	return middleware.ResponderFunc(func(w http.ResponseWriter, _ runtime.Producer) {
		w.WriteHeader(GetStatedbDumpOKCode)
		enc := gob.NewEncoder(w)
		for _, obj, ok := iter.Next(); ok; _, obj, ok = iter.Next() {
			if err := enc.Encode(obj.revision); err != nil {
				return
			}
			if err := enc.Encode(obj.data); err != nil {
				return
			}
		}
	})

}

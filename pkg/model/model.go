// Copyright 2020 The SQLFlow Authors. All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"sqlflow.org/sqlflow/pkg/database"
	"sqlflow.org/sqlflow/pkg/ir"

	pb "sqlflow.org/sqlflow/pkg/proto"
	"sqlflow.org/sqlflow/pkg/sqlfs"
)

const modelZooDB = "sqlflow"
const modelZooTable = "sqlflow.trained_models"

// Model represent a trained model, which could be saved to a filesystem or sqlfs.
type Model struct {
	workDir     string // We don't expose and gob workDir; instead we tar it.
	TrainSelect string // TrainSelect is gob-encoded during I/O.
}

// New an empty model.
func New(cwd, trainSelect string) *Model {
	return &Model{
		workDir:     cwd,
		TrainSelect: trainSelect}
}

// Save all files in workDir as a tarball to a filesystem or sqlfs.
func (m *Model) Save(modelURI string, trainStmt *ir.TrainStmt, session *pb.Session) error {
	if strings.Contains(modelURI, "://") {
		uriParts := strings.Split(modelURI, "://")
		if len(uriParts) == 2 {
			// oss:// or file://
			if uriParts[0] == "file" {
				dir, file := path.Split(uriParts[1])
				return m.saveTar(dir, file)
			} else if uriParts[0] == "oss" {
				return fmt.Errorf("save model to oss is not supported now")
			}
		} else {
			return fmt.Errorf("error modelURI format: %s", modelURI)
		}
	}
	db, err := database.OpenAndConnectDB(session.DbConnStr)
	if err != nil {
		return err
	}
	if err := m.saveDB(db, modelURI, session); err != nil {
		return err
	}
	return nil
}

// Load untar a saved model to a directory on the local filesystem.
// When dst=="", we do not untar model data, just extract the model meta
func Load(modelURI, dst string, db *database.DB) (*Model, error) {
	// FIXME(typhoonzero): unify arguments with save, use session,
	// so that can pass oss credentials too.
	if strings.Contains(modelURI, "://") {
		uriParts := strings.Split(modelURI, "://")
		if len(uriParts) == 2 {
			// oss:// or file://
			if uriParts[0] == "file" {
				dir, file := path.Split(uriParts[1])
				return loadTar(dir, dst, file)
			} else if uriParts[0] == "oss" {
				return nil, fmt.Errorf("load model from oss is not supported now")
			}
		} else {
			return nil, fmt.Errorf("error modelURI format: %s", modelURI)
		}
	}
	return loadDB(db, modelURI, dst)
}

// saveDB creates a sqlfs table if it doesn't yet exist, and writes the
// train select statement into the table, followed by the tar-gzipped
// SQLFlow working directory, which contains the TensorFlow working
// directory and the trained TensorFlow model.
func (m *Model) saveDB(db *database.DB, table string, session *pb.Session) (e error) {
	sqlf, e := sqlfs.Create(db.DB, db.DriverName, table, session)
	if e != nil {
		return fmt.Errorf("cannot create sqlfs file %s: %v", table, e)
	}
	defer sqlf.Close()

	// Use a bytes.Buffer as the gob message container to separate
	// the message from the following tarball.
	var buf bytes.Buffer
	if e := gob.NewEncoder(&buf).Encode(m); e != nil {
		return fmt.Errorf("model.save: gob-encoding model failed: %v", e)
	}
	if _, e := buf.WriteTo(sqlf); e != nil {
		return fmt.Errorf("model.save: write the buffer failed: %v", e)
	}
	cmd := exec.Command("tar", "czf", "-", "-C", m.workDir, ".")
	cmd.Stdout = sqlf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if e := cmd.Run(); e != nil {
		return fmt.Errorf("tar stderr: %v\ntar cmd %v", errBuf.String(), e)
	}

	if e := sqlf.Close(); e != nil {
		return fmt.Errorf("close sqlfs error: %v", e)
	}
	return nil
}

func (m *Model) saveTar(modelDir, save string) (e error) {
	gobFile := filepath.Join(m.workDir, save+".gob")
	if e := writeGob(gobFile, m); e != nil {
		return e
	}
	modelFile := filepath.Join(modelDir, save+".tar.gz")
	cmd := exec.Command("tar", "czf", modelFile, "-C", m.workDir, ".")
	return cmd.Run()
}

func loadTar(modelDir, cwd, save string) (m *Model, e error) {
	tarFile := filepath.Join(modelDir, save+".tar.gz")
	cmd := exec.Command("tar", "zxf", tarFile, "-C", cwd)
	if e = cmd.Run(); e != nil {
		return nil, fmt.Errorf("load tar file(%s) failed: %v", tarFile, e)
	}
	gobFile := filepath.Join(cwd, save+".gob")
	m = &Model{}
	if e = readGob(gobFile, m); e != nil {
		return nil, e
	}
	return m, nil
}

// load reads from the given sqlfs table for the train select
// statement, and untar the SQLFlow working directory, which contains
// the TensorFlow model, into directory cwd if cwd is not "".
func loadDB(db *database.DB, table, cwd string) (m *Model, e error) {
	sqlf, e := sqlfs.Open(db.DB, table)
	if e != nil {
		return nil, fmt.Errorf("cannot open sqlfs file %s: %v", table, e)
	}
	defer sqlf.Close()

	var buf bytes.Buffer
	if _, e := buf.ReadFrom(sqlf); e != nil {
		return nil, fmt.Errorf("buf.ReadFrom %v", e)
	}
	m = &Model{}
	if e := gob.NewDecoder(&buf).Decode(m); e != nil {
		return nil, fmt.Errorf("gob-decoding train select failed: %v", e)
	}

	if cwd != "" { // empty in invalid param for tar -C
		cmd := exec.Command("tar", "xzf", "-", "-C", cwd)
		cmd.Stdin = &buf
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("tar %v", string(output))
		}
	}
	return m, nil
}

func writeGob(filePath string, object interface{}) error {
	file, e := os.Create(filePath)
	if e != nil {
		return fmt.Errorf("create gob file :%s, error: %v", filePath, e)
	}
	defer file.Close()
	if e := gob.NewEncoder(file).Encode(object); e != nil {
		return fmt.Errorf("model.save: gob-encoding model failed: %v", e)
	}
	return nil
}

func readGob(filePath string, object interface{}) error {
	file, e := os.Open(filePath)
	if e != nil {
		return fmt.Errorf("model.load: gob-decoding model failed: %v", e)
	}
	defer file.Close()
	if e := gob.NewDecoder(file).Decode(object); e != nil {
		return fmt.Errorf("model.load: gob-decoding model failed: %v", e)
	}
	return nil
}

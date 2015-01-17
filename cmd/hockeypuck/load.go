/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012-2014  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

// hockeypuck is an OpenPGP keyserver.
package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/hockeypuck/conflux.v2"
	"gopkg.in/hockeypuck/conflux.v2/recon"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"launchpad.net/gnuflag"

	. "gopkg.in/hockeypuck/hockeypuck.v2"
	"gopkg.in/hockeypuck/hockeypuck.v2/openpgp"
)

type loadCmd struct {
	configuredCmd
	path            string
	txnSize         int
	ignoreDups      bool
	verifyRoundTrip bool

	db    *openpgp.DB
	w     *openpgp.Worker
	ptree recon.PrefixTree
	nkeys int
	tx    *sqlx.Tx
}

func (ec *loadCmd) Name() string { return "load" }

func (ec *loadCmd) Desc() string { return "Load OpenPGP keyring files into database" }

func newLoadCmd() *loadCmd {
	cmd := new(loadCmd)
	flags := gnuflag.NewFlagSet(cmd.Name(), gnuflag.ExitOnError)
	flags.StringVar(&cmd.configPath, "config", "", "Hockeypuck configuration file")
	flags.StringVar(&cmd.path, "path", "", "OpenPGP keyring file path or glob pattern")
	flags.IntVar(&cmd.txnSize, "txn-size", 5000, "Transaction size; public keys per commit")
	flags.BoolVar(&cmd.ignoreDups, "ignore-dups", false, "Ignore duplicate entries")
	flags.BoolVar(&cmd.verifyRoundTrip, "verify-round-trip", false, "Fetch key after insert and verify digest (slow)")
	cmd.flags = flags
	return cmd
}

func (ec *loadCmd) Main() {
	if ec.path == "" {
		Usage(ec, "--path is required")
	}
	if ec.verifyRoundTrip {
		ec.txnSize = 1
	}
	if ec.txnSize < 1 {
		Usage(ec, "Invalid --txn-size, must be >= 1")
	}
	ec.configuredCmd.Main()
	InitLog()
	var err error
	if ec.db, err = openpgp.NewDB(); err != nil {
		die(err)
	}
	ec.w = &openpgp.Worker{Loader: openpgp.NewLoader(ec.db, true)}
	// Ensure tables all exist
	if err = ec.db.CreateTables(); err != nil {
		die(err)
	}
	reconSettings := recon.NewSettings(openpgp.Config().Settings.TomlTree)
	if ec.ptree, err = openpgp.NewSksPTree(reconSettings); err != nil {
		die(err)
	}
	// Create the prefix tree (if not exists)
	if err = ec.ptree.Create(); err != nil {
		die(fmt.Errorf("Unable to create prefix tree: %v", err))
	}
	// Ensure tables all exist
	if err = ec.db.CreateTables(); err != nil {
		die(fmt.Errorf("Unable to create database tables: %v", err))
	}
	// Load all keys from input material
	ec.loadAllKeys(ec.path)
	// Close the prefix tree
	if err = ec.ptree.Close(); err != nil {
		log.Println("Close ptree:", err)
	}
	// Close the database connection
	if err = ec.db.Close(); err != nil {
		log.Println("Close database:", err)
	}
}

func (ec *loadCmd) flushDb() {
	if ec.tx != nil {
		if !ec.verifyRoundTrip {
			log.Println("Loaded", ec.nkeys, "keys")
		}
		if err := ec.tx.Commit(); err != nil {
			die(fmt.Errorf("Error committing transaction: %v", err))
		}
		ec.tx = nil
		ec.nkeys = 0
	}
}

func (ec *loadCmd) insertKey(keyRead *openpgp.ReadKeyResult) error {
	var err error
	if ec.tx == nil {
		if ec.tx, err = ec.w.Begin(); err != nil {
			die(fmt.Errorf("Error starting new transaction: %v", err))
		}
	} else if ec.nkeys%ec.txnSize == 0 {
		ec.flushDb()
		if ec.tx, err = ec.w.Begin(); err != nil {
			die(fmt.Errorf("Error starting new transaction: %v", err))
		}
	}
	// Load key into relational database
	if err = ec.w.InsertKeyTx(ec.tx, keyRead.Pubkey); err != nil {
		log.Println("Error inserting key:", keyRead.Pubkey.Fingerprint(), ":", err)
		if _, is := err.(pq.Error); is {
			die(fmt.Errorf("Unable to load due to database errors."))
		}
	}
	ec.nkeys++

	if ec.verifyRoundTrip {
		ec.flushDb()
		loadKey := keyRead.Pubkey
		loadDigest := openpgp.SksDigest(loadKey, md5.New())
		if loadKey.Md5 != loadDigest {
			log.Println("RTC: loaded key", loadKey.Md5, "!=", "recalculated", loadDigest)
		}
		checkKey, err := ec.w.FetchKey(loadKey.RFingerprint)
		if err != nil {
			log.Println("RTC: check failed for", loadKey.Fingerprint(), ":", err)
			return err
		}
		checkDigest := openpgp.SksDigest(checkKey, md5.New())
		if checkKey.Md5 != checkDigest {
			log.Println("RTC: check key", checkKey.Md5, "!=", "recalculated", checkDigest)
		}
		if loadKey.Md5 != checkKey.Md5 {
			log.Println("RTC: load key", loadKey.Md5, "!=", "check key", checkKey.Md5)
		}
	}
	return err
}

func (ec *loadCmd) loadAllKeys(path string) {
	keyfiles, err := filepath.Glob(path)
	if err != nil {
		die(err)
	}
	for _, keyfile := range keyfiles {
		var f *os.File
		if f, err = os.Open(keyfile); err != nil {
			log.Println("Failed to open", keyfile, ":", err)
			continue
		}
		defer f.Close()
		log.Println("Loading keys from", keyfile)
		defer ec.flushDb()
		for keyRead := range openpgp.ReadKeys(f) {
			if keyRead.Error != nil {
				log.Println("Error reading key:", keyRead.Error)
				continue
			}
			digest, err := hex.DecodeString(keyRead.Pubkey.Md5)
			if err != nil {
				log.Println("bad digest:", keyRead.Pubkey.Md5)
				continue
			}
			digest = recon.PadSksElement(digest)
			digestZp := conflux.Zb(conflux.P_SKS, digest)
			err = ec.ptree.Insert(digestZp)
			if err != nil {
				log.Println("Error inserting digest ", keyRead.Pubkey.Md5, ":", err)
				continue
			}
			err = ec.insertKey(keyRead)
			if err != nil {
				log.Println("Error inserting key", keyRead.Pubkey.Md5, "into database:", err)
				// Attempt to remove digest from ptree, since it was not successfully loaded
				ec.ptree.Remove(digestZp)
				continue
			}
		}
	}
}

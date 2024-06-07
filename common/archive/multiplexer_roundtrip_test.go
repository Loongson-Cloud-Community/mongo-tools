// Copyright (C) MongoDB, Inc. 2014-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package archive

import (
	"bytes"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/testtype"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

const testDocCount = 10000

var testIntents = []*intents.Intent{
	{
		DB:       "foo",
		C:        "bar",
		Location: "foo.bar",
	},
	{
		DB:       "ding",
		C:        "bats",
		Location: "ding.bats",
	},
	{
		DB:       "flim",
		C:        "flam.fooey",
		Location: "flim.flam.fooey",
	},
	{
		DB:       "crow",
		C:        "bar",
		Location: "crow.bar",
	},
}

type testDoc struct {
	Bar int
	Baz string
}

type closingBuffer struct {
	bytes.Buffer
}

func (*closingBuffer) Close() error {
	return nil
}

type testNotifier struct{}

func (n *testNotifier) Notify() {}

func TestBasicMux(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.UnitTestType)

	var err error

	Convey("with thousands of docs in each of five collections", t, func() {
		buf := &closingBuffer{bytes.Buffer{}}

		mux := NewMultiplexer(buf, new(testNotifier))
		muxIns := map[string]*MuxIn{}

		inChecksum := map[string]hash.Hash{}
		inLengths := map[string]*int{}
		outChecksum := map[string]hash.Hash{}
		outLengths := map[string]*int{}

		// To confirm that what we multiplex is the same as what we demultiplex, we
		// create input and output hashes for each namespace. After we finish
		// multiplexing and demultiplexing we will compare all of the CRCs for each
		// namespace
		errChan := make(chan error)
		makeIns(testIntents, mux, inChecksum, muxIns, inLengths, errChan)

		Convey("each document should be multiplexed", func() {
			go mux.Run()

			for range testIntents {
				err := <-errChan
				So(err, ShouldBeNil)
			}
			close(mux.Control)
			err = <-mux.Completed
			So(err, ShouldBeNil)

			demux := &Demultiplexer{
				In:              buf,
				NamespaceStatus: make(map[string]int),
			}
			demuxOuts := map[string]*RegularCollectionReceiver{}

			errChan := make(chan error)
			makeOuts(testIntents, demux, outChecksum, demuxOuts, outLengths, errChan)

			Convey("and demultiplexed successfully", func() {
				So(demux.Run(), ShouldBeNil)
				So(err, ShouldBeNil)

				for range testIntents {
					err := <-errChan
					So(err, ShouldBeNil)
				}
				for _, dbc := range testIntents {
					ns := dbc.Namespace()
					So(*inLengths[ns], ShouldEqual, *outLengths[ns])
					inSum := inChecksum[ns].Sum([]byte{})
					outSum := outChecksum[ns].Sum([]byte{})
					So(inSum, ShouldResemble, outSum)
				}
			})
		})
	})
	return
}

func TestParallelMux(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.UnitTestType)

	Convey("parallel mux/demux over a pipe", t, func() {
		readPipe, writePipe, err := os.Pipe()
		So(err, ShouldBeNil)

		mux := NewMultiplexer(writePipe, new(testNotifier))
		muxIns := map[string]*MuxIn{}

		demux := &Demultiplexer{
			In:              readPipe,
			NamespaceStatus: make(map[string]int),
		}
		demuxOuts := map[string]*RegularCollectionReceiver{}

		inChecksum := map[string]hash.Hash{}
		inLengths := map[string]*int{}

		outChecksum := map[string]hash.Hash{}
		outLengths := map[string]*int{}

		writeErrChan := make(chan error)
		readErrChan := make(chan error)

		makeIns(testIntents, mux, inChecksum, muxIns, inLengths, writeErrChan)
		makeOuts(testIntents, demux, outChecksum, demuxOuts, outLengths, readErrChan)

		go func() {
			So(demux.Run(), ShouldBeNil)
		}()
		go mux.Run()

		for range testIntents {
			err := <-writeErrChan
			So(err, ShouldBeNil)
			err = <-readErrChan
			So(err, ShouldBeNil)
		}
		close(mux.Control)
		muxErr := <-mux.Completed
		So(muxErr, ShouldBeNil)

		for _, dbc := range testIntents {
			ns := dbc.Namespace()
			So(*inLengths[ns], ShouldEqual, *outLengths[ns])
			inSum := inChecksum[ns].Sum([]byte{})
			outSum := outChecksum[ns].Sum([]byte{})
			So(inSum, ShouldResemble, outSum)
		}
	})
	return
}

func makeIns(testIntents []*intents.Intent, mux *Multiplexer, inChecksum map[string]hash.Hash, muxIns map[string]*MuxIn, inLengths map[string]*int, errCh chan<- error) {
	for index, dbc := range testIntents {
		ns := dbc.Namespace()
		sum := crc32.NewIEEE()
		muxIn := &MuxIn{Intent: dbc, Mux: mux}
		inLength := 0

		inChecksum[ns] = sum
		muxIns[ns] = muxIn
		inLengths[ns] = &inLength

		go func(index int) {
			err := muxIn.Open()
			if err != nil {
				errCh <- err
				return
			}
			staticBSONBuf := make([]byte, db.MaxBSONSize)
			for i := 0; i < testDocCount; i++ {
				bsonBytes, _ := bson.Marshal(testDoc{Bar: index * i, Baz: ns})
				bsonBuf := staticBSONBuf[:len(bsonBytes)]
				copy(bsonBuf, bsonBytes)
				_, err := muxIn.Write(bsonBuf)
				So(err, ShouldBeNil)
				sum.Write(bsonBuf)
				inLength += len(bsonBuf)
			}
			err = muxIn.Close()
			errCh <- err
		}(index)
	}
}

func makeOuts(testIntents []*intents.Intent, demux *Demultiplexer, outChecksum map[string]hash.Hash, demuxOuts map[string]*RegularCollectionReceiver, outLengths map[string]*int, errCh chan<- error) {
	for _, dbc := range testIntents {
		ns := dbc.Namespace()
		sum := crc32.NewIEEE()
		muxOut := &RegularCollectionReceiver{
			Intent: dbc,
			Demux:  demux,
			Origin: ns,
		}
		outLength := 0

		outChecksum[ns] = sum
		demuxOuts[ns] = muxOut
		outLengths[ns] = &outLength

		So(demuxOuts[ns].Open(), ShouldBeNil)
		go func() {
			bs := make([]byte, db.MaxBSONSize)
			var err error
			for {
				var length int
				length, err = muxOut.Read(bs)
				if err != nil {
					muxOut.Close()
					break
				}
				sum.Write(bs[:length])
				outLength += len(bs[:length])
			}
			if err == io.EOF {
				err = nil
			}
			errCh <- err
		}()
	}
}

func buildSingleIntentArchive(t *testing.T, singleIntent *intents.Intent) *closingBuffer {
	var err error

	buf := &closingBuffer{bytes.Buffer{}}

	mux := NewMultiplexer(buf, new(testNotifier))
	muxIns := map[string]*MuxIn{}

	inChecksum := map[string]hash.Hash{}
	inLengths := map[string]*int{}
	errChan := make(chan error)

	makeIns([]*intents.Intent{singleIntent}, mux, inChecksum, muxIns, inLengths, errChan)

	go mux.Run()

	err = <-errChan
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	close(mux.Control)
	err = <-mux.Completed
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	return buf
}

func TestTOOLS1826(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.UnitTestType)
	require := require.New(t)

	singleIntent := testIntents[0]

	demux := &Demultiplexer{
		In:              buildSingleIntentArchive(t, singleIntent),
		NamespaceStatus: make(map[string]int),
	}

	muxOut := &RegularCollectionReceiver{
		Intent: singleIntent,
		Demux:  demux,
		Origin: singleIntent.Namespace(),
	}
	require.NoError(muxOut.Open())

	var demuxErr error
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		demuxErr = demux.Run()
	}()

	// Closing the receiver before reading shouldn't panic.
	muxOut.Close()

	wg.Wait()
	require.ErrorIs(demuxErr, errInterrupted)
}

func TestTOOLS2403(t *testing.T) {
	testtype.SkipUnlessTestType(t, testtype.UnitTestType)
	require := require.New(t)

	singleIntent := testIntents[0]

	demux := &Demultiplexer{
		In:              buildSingleIntentArchive(t, singleIntent),
		NamespaceStatus: make(map[string]int),
	}

	muxOut := &RegularCollectionReceiver{
		Intent: singleIntent,
		Demux:  demux,
		Origin: singleIntent.Namespace(),
	}
	require.NoError(muxOut.Open())

	var demuxErr error
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		demuxErr = demux.Run()
	}()

	// Read all the documents, but don't read past into EOF.
	bs := make([]byte, db.MaxBSONSize)
	for i := 0; i < testDocCount; i++ {
		_, err := muxOut.Read(bs)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Closing the intent before reading EOF should not deadlock.
	muxOut.Close()

	wg.Wait()
	require.NoError(demuxErr)

	return
}

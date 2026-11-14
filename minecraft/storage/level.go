/*
 * Copyright (c) 2026 Gilles Chehade <gilles@poolp.org>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package storage

import (
	"compress/gzip"
	"os"
	"time"

	"github.com/Tnze/go-mc/nbt"
	"github.com/Tnze/go-mc/save"
)

// levelNBT is the level.dat of a world made for the store: a void
// overworld in adventure mode where nothing ticks, burns, spawns or
// snows, so that what the store wrote stays as written.
type levelNBT struct {
	Data levelDataNBT `nbt:"Data"`
}

type levelDataNBT struct {
	AllowCommands                byte `nbt:"allowCommands"`
	BorderCenterX, BorderCenterZ float64
	BorderDamagePerBlock         float64
	BorderSafeZone               float64
	BorderSize                   float64
	BorderSizeLerpTarget         float64
	BorderSizeLerpTime           int64
	BorderWarningBlocks          float64
	BorderWarningTime            float64
	ClearWeatherTime             int32 `nbt:"clearWeatherTime"`
	DataVersion                  int32
	DayTime                      int64
	Difficulty                   byte
	DifficultyLocked             bool
	GameRules                    map[string]string
	WorldGenSettings             save.WorldGenSettings
	GameType                     int32
	HardCore                     bool `nbt:"hardcore"`
	Initialized                  bool `nbt:"initialized"`
	LastPlayed                   int64
	LevelName                    string
	MapFeatures                  bool
	Raining                      bool  `nbt:"raining"`
	RainTime                     int32 `nbt:"rainTime"`
	SpawnAngle                   float32
	SpawnX, SpawnY, SpawnZ       int32
	Thundering                   bool  `nbt:"thundering"`
	ThunderTime                  int32 `nbt:"thunderTime"`
	Time                         int64
	Version                      levelVersionNBT
	StorageVersion               int32 `nbt:"version"`
	WasModded                    bool
}

type levelVersionNBT struct {
	ID       int32 `nbt:"Id"`
	Name     string
	Series   string
	Snapshot byte
}

// gameRules are what keeps the world as the store wrote it.  Players
// are kept out of it by force-gamemode and gamemode=adventure in
// server.properties, which are the server's to set.
var gameRules = map[string]string{
	"randomTickSpeed":    "0",
	"doFireTick":         "false",
	"mobGriefing":        "false",
	"doMobSpawning":      "false",
	"doWeatherCycle":     "false",
	"doVinesSpread":      "false",
	"doTraderSpawning":   "false",
	"doPatrolSpawning":   "false",
	"doWardenSpawning":   "false",
	"doInsomnia":         "false",
	"spawnRadius":        "0",
	"commandBlockOutput": "false",
}

func newLevel(name string) *levelNBT {
	const border = 59999968
	return &levelNBT{Data: levelDataNBT{
		BorderDamagePerBlock: 0.2,
		BorderSafeZone:       5,
		BorderSize:           border,
		BorderSizeLerpTarget: border,
		BorderWarningBlocks:  5,
		BorderWarningTime:    15,
		ClearWeatherTime:     1 << 30,
		DataVersion:          dataVersion,
		DayTime:              6000,
		Difficulty:           0,
		GameRules:            gameRules,
		WorldGenSettings: save.WorldGenSettings{
			Seed: 0,
			Dimensions: map[string]save.DimensionGenerator{
				"minecraft:overworld": {
					Type: "minecraft:overworld",
					Generator: map[string]any{
						"type": "minecraft:flat",
						"settings": map[string]any{
							"layers":   []map[string]any{},
							"biome":    "minecraft:the_void",
							"features": false,
							"lakes":    false,
						},
					},
				},
				"minecraft:the_nether": save.DefaultDimensionsGenerators["minecraft:the_nether"],
				"minecraft:the_end":    save.DefaultDimensionsGenerators["minecraft:the_end"],
			},
		},
		GameType:       2, // adventure
		Initialized:    true,
		LastPlayed:     time.Now().UnixMilli(),
		LevelName:      name,
		SpawnX:         8,
		SpawnY:         baseY + layers + 1,
		SpawnZ:         8,
		Time:           6000,
		Version:        levelVersionNBT{ID: dataVersion, Name: "1.20.1", Series: "main"},
		StorageVersion: 19133,
	}}
}

// writeLevel writes a level.dat, gzip compressed NBT as the game has it.
func writeLevel(path, name string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if err := nbt.NewEncoder(zw).Encode(newLevel(name), ""); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

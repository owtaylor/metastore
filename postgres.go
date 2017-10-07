package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/docker/distribution/digest"
	_ "github.com/lib/pq"
	"log"
	"sort"
	"time"
)

type postgresDatabase struct {
	db *sql.DB
}

type postgresTransaction struct {
	tx               *sql.Tx
	modify           bool
	modificationTime time.Time
}

func NewPostgresDB(url string) (Database, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}

	return &postgresDatabase{
		db: db,
	}, nil
}

func (pdb *postgresDatabase) Begin(ctx context.Context) (Tx, error) {
	ptx := postgresTransaction{}

	var err error
	ptx.tx, err = pdb.db.BeginTx(ctx, nil)
	return ptx, err
}

func (pdb *postgresDatabase) DoQuery(ctx context.Context, query *Query) ([]*Repository, error) {
	tx, err := pdb.Begin(ctx)
	if err != nil {
		return nil, err
	}

	results, err := tx.DoQuery(query)
	if err != nil {
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, err
	}

	return results, nil
}

func (pdb *postgresDatabase) ModificationTime() (time.Time, error) {
	var t time.Time
	err := pdb.db.QueryRow(
		`SELECT ModificationTime FROM modification`).Scan(&t)
	return t, err
}

func (ptx postgresTransaction) Commit() error {
	if ptx.modify {
		err := ptx.tx.QueryRow(
			`UPDATE modification SET ModificationTime = now() RETURNING ModificationTime`).Scan(&ptx.modificationTime)
		if err != nil {
			ptx.tx.Rollback()
			return err
		}
	}

	return ptx.tx.Commit()
}

func (ptx postgresTransaction) Rollback() error {
	return ptx.tx.Rollback()
}

func (ptx postgresTransaction) Modified() (bool, time.Time) {
	return ptx.modify, ptx.modificationTime
}

func (ptx postgresTransaction) exec(query string, args ...interface{}) (sql.Result, error) {
	res, err := ptx.tx.Exec(query, args...)

	if err != nil {
		return res, err
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return res, err
	}

	if rowsAffected > 0 {
		ptx.modify = true
	}

	return res, err
}

func (ptx postgresTransaction) doImageQuery(query *Query) ([]*Repository, error) {
	whereClause, args := makeWhereClause(query)

	imageQuery := `SELECT i.MediaType, i.Digest, i.OS, i.Arch, i.Annotations, t.Repository, t.Tag FROM image i ` +
		`JOIN imageTag t on t.Image = i.Digest ` +
		whereClause +
		` ORDER BY (t.Repository, i.Digest)`

	rows, err := ptx.tx.Query(imageQuery, args...)
	if err != nil {
		return make([]*Repository, 0), err
	}

	var result []*Repository = make([]*Repository, 0)
	var currentRepository *Repository
	var currentImage *TaggedImage
	for rows.Next() {
		var mediaType string
		var imageDigest digest.Digest
		var os string
		var arch string
		var imageAnnotationsJson []byte
		var repository string
		var tag string

		err = rows.Scan(&mediaType, &imageDigest, &os, &arch, &imageAnnotationsJson, &repository, &tag)
		if err != nil {
			return nil, err
		}
		if currentRepository == nil || repository != currentRepository.Name {
			currentRepository = &Repository{
				Name:   repository,
				Images: make([]*TaggedImage, 0),
				Lists:  make([]*TaggedImageList, 0),
			}
			result = append(result, currentRepository)
			currentImage = nil
		}
		if currentImage == nil || imageDigest != currentImage.Digest {
			currentImage = &TaggedImage{
				Image: Image{
					Digest:    imageDigest,
					MediaType: mediaType,
					OS:        os,
					Arch:      arch,
				},
				Tags: make([]string, 0),
			}
			err = json.Unmarshal(imageAnnotationsJson, &currentImage.Annotations)
			if err != nil {
				log.Print(err)
				continue
			}
			currentRepository.Images = append(currentRepository.Images, currentImage)
		}

		currentImage.Tags = append(currentImage.Tags, tag)
	}

	return result, nil
}

func (ptx postgresTransaction) doListQuery(query *Query) ([]*Repository, error) {
	whereClause, args := makeWhereClause(query)

	listQuery := `SELECT i.MediaType, i.Digest, i.OS, i.Arch, i.Annotations, t.Repository, t.Tag, l.Digest, l.MediaType, l.Annotations FROM image i ` +
		`JOIN listEntry e on e.Image = i.Digest ` +
		`JOIN listTag t on t.List = e.List ` +
		`JOIN list l on e.List = l.Digest ` +
		whereClause +
		` ORDER BY (t.Repository, l.Digest, i.Digest)`

	rows, err := ptx.tx.Query(listQuery, args...)
	if err != nil {
		return make([]*Repository, 0), err
	}

	var result []*Repository = make([]*Repository, 0)
	var currentRepository *Repository
	var currentList *TaggedImageList
	var currentImage *Image
	for rows.Next() {
		var mediaType string
		var imageDigest digest.Digest
		var os string
		var arch string
		var imageAnnotationsJson []byte
		var repository string
		var tag string
		var listDigest digest.Digest
		var listMediaType string
		var listAnnotationsJson []byte

		err = rows.Scan(&mediaType, &imageDigest, &os, &arch, &imageAnnotationsJson, &repository, &tag, &listDigest, &listMediaType, &listAnnotationsJson)
		if err != nil {
			return make([]*Repository, 0), err
		}

		if currentRepository == nil || repository != currentRepository.Name {
			currentRepository = &Repository{
				Name:   repository,
				Images: make([]*TaggedImage, 0),
				Lists:  make([]*TaggedImageList, 0),
			}
			result = append(result, currentRepository)
			currentList = nil
			currentImage = nil
		}
		if currentList == nil || listDigest != currentList.Digest {
			currentList = &TaggedImageList{
				ImageList: ImageList{
					Digest:    listDigest,
					MediaType: listMediaType,
				},
				Tags: make([]string, 0),
			}
			err = json.Unmarshal(listAnnotationsJson, &currentList.Annotations)
			if err != nil {
				log.Print(err)
				continue
			}
			currentRepository.Lists = append(currentRepository.Lists, currentList)
			currentImage = nil
		}
		if currentImage == nil || imageDigest != currentImage.Digest {
			currentImage = &Image{
				Digest:    imageDigest,
				MediaType: mediaType,
				OS:        os,
				Arch:      arch,
			}
			err = json.Unmarshal(imageAnnotationsJson, &currentImage.Annotations)
			if err != nil {
				log.Print(err)
				continue
			}

			currentList.Images = append(currentList.Images, currentImage)
		}
		if len(currentList.Images) == 1 {
			currentList.Tags = append(currentList.Tags, tag)
		}
	}

	return result, nil
}

func (ptx postgresTransaction) DoQuery(query *Query) ([]*Repository, error) {
	imageRepos, err := ptx.doImageQuery(query)
	if err != nil {
		return nil, err
	}
	listRepos, err := ptx.doListQuery(query)
	if err != nil {
		return nil, err
	}

	i := 0
	j := 0
	result := make([]*Repository, 0)
	for i < len(imageRepos) || j < len(listRepos) {
		if i < len(imageRepos) && j < len(listRepos) {
			if imageRepos[i].Name == listRepos[j].Name {
				imageRepos[i].Lists = listRepos[j].Lists
				result = append(result, imageRepos[i])
				i++
				j++
			} else if imageRepos[i].Name < listRepos[j].Name {
				result = append(result, imageRepos[i])
				i++
			} else {
				result = append(result, listRepos[j])
				j++
			}
		} else if i < len(imageRepos) {
			result = append(result, imageRepos[i])
			i++
		} else {
			result = append(result, listRepos[j])
			j++
		}
	}

	for _, repo := range result {
		for _, image := range repo.Images {
			sort.Strings(image.Tags)
		}
		for _, list := range repo.Lists {
			sort.Strings(list.Tags)
		}
	}

	return result, nil
}

func (ptx postgresTransaction) getTags(repository string, target string, dgst digest.Digest) (map[string]bool, error) {
	rows, err := ptx.tx.Query(
		`SELECT Tag FROM `+target+`Tag WHERE `+target+` = $1 `,
		dgst)
	if err != nil {
		return nil, err
	}

	result := make(map[string]bool)
	for rows.Next() {
		var tag string
		err := rows.Scan(&tag)
		if err != nil {
			return nil, err
		}
		result[tag] = true
	}

	return result, nil
}

func (ptx postgresTransaction) setTags(repository string, target string, targetUpper string, dgst digest.Digest, tags []string) error {
	log.Printf("Setting tags for %s %s/%s: %s", target, repository, dgst, tags)
	oldTags, err := ptx.getTags(repository, target, dgst)
	if err != nil {
		return err
	}

	for _, tag := range tags {
		delete(oldTags, tag)
		_, err := ptx.exec(
			`INSERT INTO `+target+`Tag (Repository, Tag, `+targetUpper+` ) `+
				`VALUES ($1, $2, $3) `+
				`ON CONFLICT (Repository, Tag) DO UPDATE SET `+targetUpper+` = $3 `,
			repository, tag, dgst)

		if err != nil {
			return err
		}
	}

	for tag := range oldTags {
		_, err := ptx.exec(
			`DELETE FROM `+target+`Tag `+
				`WHERE Repository = $1 AND Tag = $2 AND `+targetUpper+` = $3 `,
			repository, tag, dgst)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ptx postgresTransaction) SetImageTags(repository string, dgst digest.Digest, tags []string) error {
	return ptx.setTags(repository, "image", "Image", dgst, tags)
}

func (ptx postgresTransaction) SetImageListTags(repository string, dgst digest.Digest, tags []string) error {
	return ptx.setTags(repository, "list", "List", dgst, tags)
}

func (ptx postgresTransaction) storeImage(repository string, image *Image) error {
	log.Printf("Storing image %s/%s", repository, image.Digest)
	annotationsJson, _ := json.Marshal(image.Annotations)
	_, err := ptx.exec(
		`INSERT INTO image (Digest, MediaType, Arch, OS, Annotations) `+
			`VALUES ($1, $2, $3, $4, $5) ON CONFLICT (digest) DO NOTHING `,
		image.Digest, image.MediaType, image.Arch, image.OS, annotationsJson)
	return err
}

func (ptx postgresTransaction) StoreImage(repository string, image *TaggedImage) error {
	err := ptx.storeImage(repository, &image.Image)
	if err != nil {
		return err
	}

	return ptx.SetImageTags(repository, image.Digest, image.Tags)
}

func (ptx postgresTransaction) storeImageList(repository string, list *ImageList) error {
	log.Printf("Storing list %s/%s", repository, list.Digest)
	annotationsJson, _ := json.Marshal(list.Annotations)
	res, err := ptx.exec(
		`INSERT INTO list (Digest, MediaType, Annotations) `+
			`VALUES ($1, $2, $3) ON CONFLICT (Digest) DO NOTHING `,
		list.Digest, list.MediaType, annotationsJson)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return nil
	}

	for _, image := range list.Images {
		err = ptx.storeImage(repository, image)
		if err != nil {
			return err
		}

		_, err := ptx.exec(
			`INSERT INTO listEntry (List, Image) `+
				`VALUES ($1, $2) ON CONFLICT (List, Image) DO NOTHING `,
			list.Digest, image.Digest)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ptx postgresTransaction) StoreImageList(repository string, list *TaggedImageList) error {
	err := ptx.storeImageList(repository, &list.ImageList)
	if err != nil {
		return err
	}

	return ptx.SetImageListTags(repository, list.Digest, list.Tags)
}

func (ptx postgresTransaction) DeleteImage(repository string, dgst digest.Digest) error {
	log.Printf("Deleting tags for image %s/%s", repository, dgst)
	_, err := ptx.exec(
		`DELETE FROM ImageTag WHERE Repository = $1 AND Image = $2 `,
		repository, dgst)

	return err
}

func (ptx postgresTransaction) DeleteImageList(repository string, dgst digest.Digest) error {
	log.Printf("Deleting tags for image_list %s/%s", repository, dgst)
	_, err := ptx.exec(
		`DELETE FROM listTag WHERE Repository = $1 AND List = $2 `,
		repository, dgst)

	return err
}

func (ptx postgresTransaction) deleteMissingReposFromTable(table string, allRepos map[string]bool) error {
	toDelete := make([]string, 0)

	rows, err := ptx.tx.Query(`SELECT DISTINCT Repository FROM ` + table)
	if err != nil {
		return err
	}

	for rows.Next() {
		var repository string
		err := rows.Scan(&repository)
		if err != nil {
			return err
		}
		if !allRepos[repository] {
			toDelete = append(toDelete, repository)
		}
	}

	for _, repo := range toDelete {
		_, err := ptx.exec(`DELETE FROM `+table+`Tag WHERE Repository = $1`,
			repo)
		if err != nil {
			return err
		}
	}

	return err
}

func (ptx postgresTransaction) DeleteMissingRepos(allRepos map[string]bool) error {
	err := ptx.deleteMissingReposFromTable("imageTag", allRepos)
	if err != nil {
		return err
	}
	err = ptx.deleteMissingReposFromTable("listTag", allRepos)
	if err != nil {
		return err
	}

	return nil
}

func (ptx postgresTransaction) DeleteUnused() error {
	// We don't use ptx.exec() since changes here aren't really changes - they
	// affect the data we return from a query

	_, err := ptx.tx.Exec(
		`DELETE FROM list ` +
			`WHERE NOT EXISTS (SELECT * FROM listTag WHERE listTag.List = list.Digest)`)
	if err != nil {
		return err
	}

	_, err = ptx.tx.Exec(
		`DELETE FROM image ` +
			`WHERE NOT EXISTS (SELECT * FROM imageTag WHERE imageTag.Image = image.Digest) ` +
			`AND NOT EXISTS (SELECT * FROM listEntry WHERE listEntry.Image = image.Digest)`)
	if err != nil {
		return err
	}

	return nil
}

package models

import (
	"context"
	"database/sql"
	"fmt"
	"godiscourse/internal/durable"
	"godiscourse/internal/session"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	hashids "github.com/speps/go-hashids"
)

// Topic related CONST
const (
	minTitleSize = 3
	LIMIT        = 50
)

const topicsDDL = `
CREATE TABLE IF NOT EXISTS topics (
	topic_id              VARCHAR(36) PRIMARY KEY,
	short_id              VARCHAR(255) NOT NULL,
	title                 VARCHAR(512) NOT NULL,
	body                  TEXT NOT NULL,
	comments_count        INTEGER NOT NULL DEFAULT 0,
	category_id           VARCHAR(36) NOT NULL,
	user_id               VARCHAR(36) NOT NULL REFERENCES users ON DELETE CASCADE,
	score                 INTEGER NOT NULL DEFAULT 0,
	created_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
	updated_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX ON topics (short_id);
CREATE INDEX ON topics (created_at DESC);
CREATE INDEX ON topics (user_id, created_at DESC);
CREATE INDEX ON topics (category_id, created_at DESC);
CREATE INDEX ON topics (score DESC, created_at DESC);
`

var topicColumns = []string{"topic_id", "short_id", "title", "body", "comments_count", "category_id", "user_id", "score", "created_at", "updated_at"}

func (t *Topic) values() []interface{} {
	return []interface{}{t.TopicID, t.ShortID, t.Title, t.Body, t.CommentsCount, t.CategoryID, t.UserID, t.Score, t.CreatedAt, t.UpdatedAt}
}

// Topic is what use talking about
type Topic struct {
	TopicID       string
	ShortID       string
	Title         string
	Body          string
	CommentsCount int64
	CategoryID    string
	UserID        string
	Score         int
	CreatedAt     time.Time
	UpdatedAt     time.Time

	User     *User
	Category *Category
}

//CreateTopic create a new Topic
func (user *User) CreateTopic(mctx *Context, title, body, categoryID string) (*Topic, error) {
	ctx := mctx.context
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if len(title) < minTitleSize {
		return nil, session.BadDataError(ctx)
	}

	t := time.Now()
	topic := &Topic{
		TopicID:   uuid.Must(uuid.NewV4()).String(),
		Title:     title,
		Body:      body,
		UserID:    user.UserID,
		CreatedAt: t,
		UpdatedAt: t,
	}
	var err error
	topic.ShortID, err = generateShortID("topics", t)
	if err != nil {
		return nil, session.ServerError(ctx, err)
	}

	err = mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		category, err := findCategory(ctx, tx, categoryID)
		if err != nil {
			return err
		}
		if category == nil {
			return session.BadDataError(ctx)
		}
		topic.CategoryID = category.CategoryID
		category.LastTopicID = sql.NullString{String: topic.TopicID, Valid: true}
		count, err := topicsCountByCategory(ctx, tx, category.CategoryID)
		if err != nil {
			return err
		}
		category.TopicsCount, category.UpdatedAt = count+1, time.Now()
		cols, params := durable.PrepareColumnsWithValues(topicColumns)
		_, err = tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO topics(%s) VALUES (%s)", cols, params), topic.values()...)
		if err != nil {
			return err
		}
		ccols, cparams := durable.PrepareColumnsWithValues([]string{"last_topic_id", "topics_count", "updated_at"})
		cvals := []interface{}{category.LastTopicID, category.TopicsCount, category.UpdatedAt}
		_, err = tx.ExecContext(ctx, fmt.Sprintf("UPDATE categories SET (%s)=(%s) WHERE category_id='%s'", ccols, cparams, category.CategoryID), cvals...)
		if err != nil {
			return err
		}
		_, err = upsertStatistic(ctx, tx, "topics")
		return err
	})
	if err != nil {
		if _, ok := err.(session.Error); ok {
			return nil, err
		}
		return nil, session.TransactionError(ctx, err)
	}
	return topic, nil
}

// UpdateTopic update a Topic by ID
func (user *User) UpdateTopic(mctx *Context, id, title, body, categoryID string) (*Topic, error) {
	ctx := mctx.context
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title != "" && len(title) < minTitleSize {
		return nil, session.BadDataError(ctx)
	}

	var topic *Topic
	var prevCategoryID string
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		topic, err = findTopic(ctx, tx, id)
		if err != nil {
			return err
		} else if topic == nil {
			return nil
		} else if topic.UserID != user.UserID && !user.isAdmin() {
			return session.AuthorizationError(ctx)
		}
		if title != "" {
			topic.Title = title
		}
		topic.Body = body
		if categoryID != "" && topic.CategoryID != categoryID {
			prevCategoryID = topic.CategoryID
			category, err := findCategory(ctx, tx, categoryID)
			if err != nil {
				return err
			} else if category == nil {
				return session.BadDataError(ctx)
			}
			topic.CategoryID = category.CategoryID
			topic.Category = category
		}
		cols, params := durable.PrepareColumnsWithValues([]string{"title", "body", "category_id"})
		vals := []interface{}{topic.Title, topic.Body, topic.CategoryID}
		_, err = tx.ExecContext(ctx, fmt.Sprintf("UPDATE topics SET (%s)=(%s) WHERE topic_id='%s'", cols, params, topic.TopicID), vals...)
		return err
	})
	if err != nil {
		if _, ok := err.(session.Error); ok {
			return nil, err
		}
		return nil, session.TransactionError(ctx, err)
	}
	if topic == nil {
		return nil, session.NotFoundError(ctx)
	}
	if prevCategoryID != "" {
		go dispersalCategory(mctx, prevCategoryID)
		go dispersalCategory(mctx, topic.CategoryID)
	}
	topic.User = user
	return topic, nil
}

//ReadTopic read a topic by ID
func ReadTopic(mctx *Context, id string) (*Topic, error) {
	ctx := mctx.context
	var topic *Topic
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		topic, err = findTopic(ctx, tx, id)
		if err != nil {
			return err
		}
		if topic == nil {
			subs := strings.Split(id, "-")
			if len(subs) < 1 || len(subs[0]) <= 5 {
				return nil
			}
			id = subs[0]
			topic, err = findTopicByShortID(ctx, tx, id)
			if topic == nil || err != nil {
				return err
			}
		}
		user, err := findUserByID(ctx, tx, topic.UserID)
		if err != nil {
			return err
		}
		category, err := findCategory(ctx, tx, topic.CategoryID)
		if err != nil {
			return err
		}
		topic.User = user
		topic.Category = category
		return nil
	})
	if err != nil {
		return nil, session.TransactionError(ctx, err)
	}
	return topic, nil
}

func findTopic(ctx context.Context, tx *sql.Tx, id string) (*Topic, error) {
	if _, err := uuid.FromString(id); err != nil {
		return nil, nil
	}
	row := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM topics WHERE topic_id=$1", strings.Join(topicColumns, ",")), id)
	t, err := topicFromRows(row)
	if sql.ErrNoRows == err {
		return nil, nil
	}
	return t, err
}

// ReadTopicByShortID read a topic by Short ID
func ReadTopicByShortID(mctx *Context, id string) (*Topic, error) {
	subs := strings.Split(id, "-")
	if len(subs) < 1 || len(subs[0]) <= 5 {
		return nil, nil
	}
	id = subs[0]
	ctx := mctx.context
	var topic *Topic
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		topic, err = findTopicByShortID(ctx, tx, id)
		if topic == nil || err != nil {
			return err
		}
		user, err := findUserByID(ctx, tx, topic.UserID)
		if err != nil {
			return err
		}
		category, err := findCategory(ctx, tx, topic.CategoryID)
		if err != nil {
			return err
		}
		topic.User = user
		topic.Category = category
		return nil
	})
	if err != nil {
		return nil, session.TransactionError(ctx, err)
	}
	return topic, nil
}

func findTopicByShortID(ctx context.Context, tx *sql.Tx, id string) (*Topic, error) {
	row := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM topics WHERE short_id=$1", strings.Join(topicColumns, ",")), id)
	t, err := topicFromRows(row)
	if sql.ErrNoRows == err {
		return nil, nil
	}
	return t, err
}

// ReadTopics read all topics, parameters: offset default time.Now()
func ReadTopics(mctx *Context, offset time.Time) ([]*Topic, error) {
	ctx := mctx.context
	if offset.IsZero() {
		offset = time.Now()
	}

	var topics []*Topic
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		set, err := readCategorySet(ctx, tx)
		if err != nil {
			return err
		}

		query := fmt.Sprintf("SELECT %s FROM topics WHERE created_at<$1 ORDER BY created_at DESC LIMIT $2", strings.Join(topicColumns, ","))
		rows, err := tx.QueryContext(ctx, query, offset, LIMIT)
		if err != nil {
			return err
		}
		defer rows.Close()

		userIds := []string{}
		for rows.Next() {
			topic, err := topicFromRows(rows)
			if err != nil {
				return err
			}
			userIds = append(userIds, topic.UserID)
			topic.Category = set[topic.CategoryID]
			topics = append(topics, topic)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		userSet, err := readUserSet(ctx, tx, userIds)
		if err != nil {
			return err
		}
		for i, topic := range topics {
			topics[i].User = userSet[topic.UserID]
		}
		return nil
	})
	if err != nil {
		return nil, session.TransactionError(ctx, err)
	}
	return topics, nil
}

// ReadTopics read user's topics, parameters: offset default time.Now()
func (user *User) ReadTopics(mctx *Context, offset time.Time) ([]*Topic, error) {
	ctx := mctx.context
	if offset.IsZero() {
		offset = time.Now()
	}

	var topics []*Topic
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		set, err := readCategorySet(ctx, tx)
		if err != nil {
			return err
		}
		query := fmt.Sprintf("SELECT %s FROM topics WHERE user_id=$1 AND created_at<$2 ORDER BY created_at DESC LIMIT $3", strings.Join(topicColumns, ","))
		rows, err := tx.QueryContext(ctx, query, user.UserID, offset, LIMIT)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			topic, err := topicFromRows(rows)
			if err != nil {
				return err
			}
			topic.User = user
			topic.Category = set[topic.CategoryID]
			topics = append(topics, topic)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, session.TransactionError(ctx, err)
	}
	return topics, nil
}

// ReadTopics read topics by CategoryID order by created_at DESC
func (category *Category) ReadTopics(mctx *Context, offset time.Time) ([]*Topic, error) {
	ctx := mctx.context
	if offset.IsZero() {
		offset = time.Now()
	}

	var topics []*Topic
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		query := fmt.Sprintf("SELECT %s FROM topics WHERE category_id=$1 AND created_at<$2 ORDER BY created_at DESC LIMIT $3", strings.Join(topicColumns, ","))
		rows, err := tx.QueryContext(ctx, query, category.CategoryID, offset, LIMIT)
		if err != nil {
			return err
		}
		defer rows.Close()

		userIds := []string{}
		for rows.Next() {
			topic, err := topicFromRows(rows)
			if err != nil {
				return err
			}
			userIds = append(userIds, topic.UserID)
			topic.Category = category
			topics = append(topics, topic)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		userSet, err := readUserSet(ctx, tx, userIds)
		if err != nil {
			return err
		}
		for i, topic := range topics {
			topics[i].User = userSet[topic.UserID]
		}
		return nil
	})
	if err != nil {
		return nil, session.TransactionError(ctx, err)
	}
	return topics, nil
}

func (category *Category) lastTopic(ctx context.Context, tx *sql.Tx) (*Topic, error) {
	row := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM topics WHERE category_id=$1 LIMIT 1", strings.Join(topicColumns, ",")), category.CategoryID)
	t, err := topicFromRows(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

func topicsCountByCategory(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM topics WHERE category_id=$1", id).Scan(&count)
	return count, err
}

func topicsCount(ctx context.Context, tx *sql.Tx) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM topics").Scan(&count)
	return count, err
}

func topicFromRows(row durable.Row) (*Topic, error) {
	var t Topic
	err := row.Scan(&t.TopicID, &t.ShortID, &t.Title, &t.Body, &t.CommentsCount, &t.CategoryID, &t.UserID, &t.Score, &t.CreatedAt, &t.UpdatedAt)
	return &t, err
}

func generateShortID(table string, t time.Time) (string, error) {
	hd := hashids.NewData()
	hd.MinLength = 5
	h, _ := hashids.NewWithData(hd)
	return h.EncodeInt64([]int64{t.UnixNano()})
}

// MigrateTopics should be deleted after task TODO
func MigrateTopics(mctx *Context, offset time.Time, limit int64) (int64, time.Time, error) {
	ctx := mctx.context
	if offset.IsZero() {
		offset = time.Now()
	}

	last := offset
	var count int64
	set := make(map[string]string)
	err := mctx.database.RunInTransaction(ctx, func(tx *sql.Tx) error {
		query := "SELECT topic_id,short_id,created_at FROM topics WHERE created_at<$1 ORDER BY created_at DESC LIMIT $2"
		rows, err := tx.QueryContext(ctx, query, offset, limit)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var topicID string
			var shortID sql.NullString
			var t time.Time
			err = rows.Scan(&topicID, &shortID, &t)
			if err != nil {
				return err
			}
			count++
			last = t
			if shortID.Valid {
				continue
			}
			id, _ := generateShortID("topics", last)
			set[topicID] = id
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, offset, session.TransactionError(ctx, err)
	}
	for k, v := range set {
		query := fmt.Sprintf("UPDATE topics SET short_id='%s' WHERE topic_id='%s'", v, k)
		_, err = mctx.database.ExecContext(ctx, query)
		if err != nil {
			return 0, offset, session.TransactionError(ctx, err)
		}
	}
	return count, last, nil
}

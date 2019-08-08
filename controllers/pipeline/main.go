package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/betterde/ects/config"
	"github.com/betterde/ects/internal/discover"
	"github.com/betterde/ects/internal/message"
	"github.com/betterde/ects/internal/response"
	"github.com/betterde/ects/internal/utils"
	"github.com/betterde/ects/models"
	"github.com/betterde/ects/services"
	"github.com/coreos/etcd/clientv3"
	"github.com/go-xorm/builder"
	"github.com/kataras/iris"
	"github.com/kataras/iris/mvc"
	"github.com/satori/go.uuid"
	"gopkg.in/go-playground/validator.v9"
	"log"
	"sort"
)

type (
	Controller struct {
		Service services.PipelineService
	}
	BindNodeRequest struct {
		PipelineId string   `json:"pipeline_id" validate:"required,uuid4"`
		NodesId    []string `json:"nodes_id" validate:"required"`
	}
	KillPipelineRequest struct {
		PipelineId string `json:"pipeline_id" validate:"required,uuid4"`
	}
	PutStepsRequest struct {
		PipelineId string `json:"pipeline_id" validate:"required,uuid4"`
		Origin     int    `json:"origin" validate:"numeric"`
		Current    int    `json:"current" validate:"numeric"`
	}
)

var (
	validate = validator.New()
)

// Get pipelines list
func (instance *Controller) Get(ctx iris.Context) mvc.Response {
	var (
		total int64
		err   error
	)
	search := ctx.Params().GetStringDefault("search", "")
	page, limit, start := utils.Pagination(ctx)
	pipelines := make([]models.Pipeline, 0)

	if search != "" {
		total, err = models.Engine.Where(builder.Like{"name", search}).Limit(limit, start).Desc("created_at").FindAndCount(&pipelines)
	} else {
		total, err = models.Engine.Limit(limit, start).Desc("created_at").FindAndCount(&pipelines)
	}

	if err != nil {
		return response.InternalServerError("Failed to query pipelines list", err)
	}

	return response.Success("Successful", response.Payload{
		"data": pipelines,
		"meta": &response.Meta{
			Limit: limit,
			Page:  page,
			Total: int(total),
		},
	})
}

// Create a pipeline
func (instance *Controller) Post(ctx iris.Context) mvc.Response {
	pipeline := models.Pipeline{}

	if err := ctx.ReadJSON(&pipeline); err != nil {
		return response.InternalServerError("Failed to Unmarshal JSON", err)
	}

	if err := validate.Struct(pipeline); err != nil {
		validationErrors := err.(validator.ValidationErrors)
		return response.ValidationError(message.Get("pipeline", validationErrors))
	}

	pipeline.Id = uuid.NewV4().String()
	err := pipeline.Store()
	if err != nil {
		return response.InternalServerError("Failed to create pipeline", err)
	}

	key := fmt.Sprintf("%s/%s", config.Conf.Etcd.Pipeline, pipeline.Id)

	bytes, err := json.Marshal(&pipeline)
	if err != nil {
		log.Println(err)
	}

	if _, err := discover.Client.Put(context.TODO(), key, string(bytes)); err != nil {
		log.Println(err)
	}

	if err := models.CreateLog(pipeline, utils.GetUID(ctx), "CREATE PIPELINE"); err != nil {
		return response.InternalServerError("Failed to create log", err)
	}

	return response.Success("Created successfully", response.Payload{"data": pipeline})
}

// Update pipeline attributes by id
func (instance *Controller) PutBy(id string, ctx iris.Context) mvc.Response {
	pipeline := models.Pipeline{}

	if err := ctx.ReadJSON(&pipeline); err != nil {
		return response.InternalServerError("Failed to Unmarshal JSON", err)
	}

	if err := validate.Struct(pipeline); err != nil {
		validationErrors := err.(validator.ValidationErrors)
		return response.ValidationError(message.Get("pipeline", validationErrors))
	}
	pipeline.Id = id
	err := pipeline.Update()
	if err != nil {
		return response.InternalServerError("Failed to update pipeline", err)
	}

	key := fmt.Sprintf("%s/%s", config.Conf.Etcd.Pipeline, pipeline.Id)

	bytes, err := json.Marshal(&pipeline)
	if err != nil {
		log.Println(err)
	}

	if _, err := discover.Client.Put(context.TODO(), key, string(bytes)); err != nil {
		return response.InternalServerError("Failed to delete pipeline", err)
	}

	return response.Success("Updated successfully", response.Payload{"data": pipeline})
}

// Delete pipeline by id
func (instance *Controller) DeleteBy(id string, ctx iris.Context) mvc.Response {
	pipeline := models.Pipeline{
		Id: id,
	}

	key := fmt.Sprintf("%s/%s", config.Conf.Etcd.Pipeline, pipeline.Id)

	if _, err := discover.Client.Delete(context.TODO(), key); err != nil {
		return response.InternalServerError("Failed to delete pipeline", err)
	}

	if err := pipeline.Destroy(); err != nil {
		return response.InternalServerError("Failed to delete pipeline", err)
	}
	return response.Success("Deleted successfully", response.Payload{"data": make(map[string]interface{})})
}

// Get pipeline binding nodes
func (instance *Controller) GetNodes(ctx iris.Context) mvc.Response {
	id := ctx.URLParam("pipeline_id")

	if id == "" {
		return response.ValidationError("pipeline id is required")
	}

	relations := make([]models.PipelineNodePivot, 0)

	if err := models.Engine.Where(builder.Eq{"pipeline_id": id}).Find(&relations); err != nil {
		return response.InternalServerError("Failed to query relations", err)
	}

	ids := make([]string, 0)

	for _, pivot := range relations {
		ids = append(ids, pivot.NodeId)
	}

	nodes := make([]models.Node, 0)

	if err := models.Engine.Where(builder.Eq{"id": ids}).Find(&nodes); err != nil {
		return response.InternalServerError("Failed to query relations", err)
	}

	return response.Success("Successful", response.Payload{"data": nodes})
}

// Bind pipeline to node
func (instance *Controller) PostNodes(ctx iris.Context) mvc.Response {
	params := BindNodeRequest{}

	if err := ctx.ReadJSON(&params); err != nil {
		return response.InternalServerError("Failed to Unmarshal JSON", err)
	}

	if err := validate.Struct(params); err != nil {
		validationErrors := err.(validator.ValidationErrors)
		return response.ValidationError(message.Get("pipeline", validationErrors))
	}

	if _, err := models.Engine.Where(builder.Eq{"pipeline_id": params.PipelineId}).Delete(&models.PipelineNodePivot{}); err != nil {
		return response.InternalServerError("Failed to delete pipeline and node relations", err)
	}

	relations := make([]*models.PipelineNodePivot, 0)

	for _, id := range params.NodesId {
		relations = append(relations, &models.PipelineNodePivot{
			PipelineId: params.PipelineId,
			NodeId:     id,
		})
	}

	_, err := models.Engine.Insert(relations)
	if err != nil {
		return response.InternalServerError("Failed to bind pipeline to node", err)
	}

	pipeline := &models.Pipeline{
		Id: params.PipelineId,
	}

	if _, err := models.Engine.Get(pipeline); err != nil {
		return response.InternalServerError("Failed to bind pipeline to node", err)
	}

	pipeline.Nodes = params.NodesId
	bytes, err := json.Marshal(pipeline)
	if err != nil {
		log.Println(err)
	}
	// Update etcd pipeline nodes
	key := fmt.Sprintf("%s/%s", config.Conf.Etcd.Pipeline, pipeline.Id)
	if _, err := discover.Client.Put(context.TODO(), key, string(bytes)); err != nil {
		log.Println(err)
	}

	return response.Success("Bind successfully", response.Payload{"data": relations})
}

// Get pipeline tasks
func (instance *Controller) GetTasks(ctx iris.Context) mvc.Response {
	id := ctx.URLParam("pipeline_id")

	if id == "" {
		return response.ValidationError("pipeline id is required")
	}

	relations := make([]models.PipelineTaskPivot, 0)

	if err := models.Engine.Join("INNER", "tasks", "tasks.id = pipeline_task_pivot.task_id").Where(builder.Eq{"pipeline_id": id}).Asc("step").Find(&relations); err != nil {
		return response.InternalServerError("Failed to query relations", err)
	}

	ids := make([]string, 0)

	for _, relation := range relations{
		ids = append(ids, relation.TaskId)
	}

	tasks := make([]models.Task, 0)

	if err := models.Engine.Where(builder.Eq{"id": ids}).Find(&tasks); err != nil {
		return response.InternalServerError("Failed to query relations", err)
	}

	for index, relation := range relations {
		for i, task := range tasks {
			if relation.TaskId == task.Id {
				relations[index].Task = &tasks[i]
			}
		}
	}

	return response.Success("Successful", response.Payload{"data": relations})
}

// 根据拖动顺序排序数据
func (instance *Controller) PutSteps(ctx iris.Context) mvc.Response {
	params := PutStepsRequest{}

	if err := ctx.ReadJSON(&params); err != nil {
		return response.InternalServerError("Failed to Unmarshal JSON", err)
	}

	if err := validate.Struct(params); err != nil {
		validationErrors := err.(validator.ValidationErrors)
		return response.ValidationError(message.Get("pipeline", validationErrors))
	}

	relations := make([]*models.PipelineTaskPivot, 0)

	if err := models.Engine.Join("INNER", "tasks", "tasks.id = pipeline_task_pivot.task_id").Where(builder.Eq{"pipeline_id": params.PipelineId}).Asc("step").Find(&relations); err != nil {
		return response.InternalServerError("Failed to query relations", err)
	}

	count := len(relations)

	// 从任意位置挪到第一个位置
	if params.Current == 0 && params.Origin > 0 {
		for index := 0; index <= count; index++ {
			if index < params.Origin {
				relations[index].Step += 1
				if err := relations[index].Update(); err != nil {
					return response.InternalServerError("排序失败", err)
				}
			}
		}
	}

	// 从上往下挪动
	if params.Current > params.Origin {
		for index := 0; index <= count; index++ {
			if index > params.Origin && index <= params.Current {
				relations[index].Step -= 1
				if err := relations[index].Update(); err != nil {
					return response.InternalServerError("排序失败", err)
				}
			}
		}
	}

	// 从下往上挪动
	if params.Current != 0 && params.Current < params.Origin {
		for index := 0; index <= count; index++ {
			if index >= params.Current && index < params.Origin {
				relations[index].Step += 1
				if err := relations[index].Update(); err != nil {
					return response.InternalServerError("排序失败", err)
				}
			}
		}
	}

	// 修改被移动属性的值
	relations[params.Origin].Step = params.Current + 1
	if err := relations[params.Origin].Update(); err != nil {
		return response.InternalServerError("排序失败", err)
	}

	sort.Slice(relations, func(before, after int) bool {
		return relations[before].Step < relations[after].Step
	})

	ids := make([]string, 0)

	for _, relation := range relations{
		ids = append(ids, relation.TaskId)
	}

	tasks := make([]models.Task, 0)

	if err := models.Engine.Where(builder.Eq{"id": ids}).Find(&tasks); err != nil {
		return response.InternalServerError("Failed to query relations", err)
	}

	for index, relation := range relations {
		for i, task := range tasks {
			if relation.TaskId == task.Id {
				relations[index].Task = &tasks[i]
			}
		}
	}

	return response.Success("Successful", response.Payload{"data": relations})
}

// Bind the task to pipeline
func (instance *Controller) PostTask(ctx iris.Context) mvc.Response {
	pivot := models.PipelineTaskPivot{
		Id: uuid.NewV4().String(),
	}

	if err := ctx.ReadJSON(&pivot); err != nil {
		return response.InternalServerError("Failed to Unmarshal JSON", err)
	}

	if err := validate.Struct(pivot); err != nil {
		validationErrors := err.(validator.ValidationErrors)
		return response.ValidationError(message.Get("pipeline", validationErrors))
	}

	err := pivot.Store()
	if err != nil {
		return response.InternalServerError("Failed to bind pipeline to node", err)
	}

	return response.Success("Bind successfully", response.Payload{"data": pivot})
}

// Get pipeline detail by id
func (instance *Controller) GetBy(id string) mvc.Response {
	pipeline := models.Pipeline{
		Id: id,
	}

	_, err := models.Engine.Get(&pipeline)

	if err != nil {
		return response.InternalServerError("Query pipeline on error", err)
	}

	_, err = pipeline.Build()

	if err != nil {
		return response.InternalServerError("Failed to build pipeline to string", err)
	}

	return response.Success("", response.Payload{"data": pipeline})
}

func (instance *Controller) PostKiller(ctx iris.Context) mvc.Response {
	params := KillPipelineRequest{}

	if err := ctx.ReadJSON(&params); err != nil {
		return response.InternalServerError("Failed to Unmarshal JSON", err)
	}

	if err := validate.Struct(params); err != nil {
		validationErrors := err.(validator.ValidationErrors)
		return response.ValidationError(message.Get("pipeline", validationErrors))
	}

	res, err := discover.Client.Grant(context.TODO(), 2)
	if err != nil {
		log.Println(err)
	}

	key := fmt.Sprintf("%s/%s", config.Conf.Etcd.Killer, params.PipelineId)
	if _, err := discover.Client.Put(context.TODO(), key, "pipeline", clientv3.WithLease(res.ID)); err != nil {
		log.Println(err)
	}
	return response.Success("", response.Payload{"data": make(map[string]interface{})})
}

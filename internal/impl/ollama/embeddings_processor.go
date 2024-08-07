// Copyright 2024 Redpanda Data, Inc.
//
// Licensed as a Redpanda Enterprise file under the Redpanda Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
// https://github.com/redpanda-data/connect/blob/main/licenses/rcl.md

package ollama

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	"github.com/redpanda-data/benthos/v4/public/service"
)

const (
	oepFieldText = "text"
)

func init() {
	err := service.RegisterProcessor(
		"ollama_embeddings",
		ollamaEmbeddingProcessorConfig(),
		makeOllamaEmbeddingProcessor,
	)
	if err != nil {
		panic(err)
	}
}

func ollamaEmbeddingProcessorConfig() *service.ConfigSpec {
	return service.NewConfigSpec().
		Categories("AI").
		Summary("Processor that uses the Ollama API to create vector embeddings.").
		Description(`This processor sends text to your chosen Ollama large language model (LLM) and creates vector embeddings, using the Ollama API. Vector embeddings are long arrays of numbers that represent values or objects, in this case text. 

By default this processor will start a locally installed Ollama server. Ollama can be installed by following the instructions found https://ollama.com/download[here^]. An already running Ollama server can be used by configuring `+"`"+bopFieldServerAddress+"`"+`.

For more information, see the https://ollama.com/[Ollama website^]`).
		Version("4.32.0").
		Fields(
			service.NewStringField(bopFieldServerAddress).
				Description("The address of the Ollama server to use. By default, a local Ollama server starts and runs unless you specify the address of a local or remote server.").
				Example("http://127.0.0.1:11434").
				Optional(),
			service.NewStringField(bopFieldModel).
				Description("The name of the Ollama LLM to use. For a full list of models, see the https://ollama.com/models[Ollama website].").
				Examples("nomic-embed-text", "mxbai-embed-large", "snowflake-artic-embed", "all-minilm"),
			service.NewInterpolatedStringField(oepFieldText).
				Description("The text you want to create vector embeddings for. By default, the processor submits the entire payload as a string.").
				Optional(),
		)
}

func makeOllamaEmbeddingProcessor(conf *service.ParsedConfig, mgr *service.Resources) (service.Processor, error) {
	p := ollamaEmbeddingProcessor{}
	if conf.Contains(oepFieldText) {
		pf, err := conf.FieldInterpolatedString(oepFieldText)
		if err != nil {
			return nil, err
		}
		p.text = pf
	}
	b, err := newBaseProcessor(conf, mgr)
	if err != nil {
		return nil, err
	}
	p.baseOllamaProcessor = b
	return &p, nil
}

type ollamaEmbeddingProcessor struct {
	*baseOllamaProcessor

	text *service.InterpolatedString
}

func (o *ollamaEmbeddingProcessor) Process(ctx context.Context, msg *service.Message) (service.MessageBatch, error) {
	p, err := o.computeText(msg)
	if err != nil {
		return nil, err
	}
	e, err := o.generateEmbedding(ctx, p)
	if err != nil {
		return nil, err
	}
	m := msg.Copy()
	s := make([]any, len(e))
	for i, f := range e {
		s[i] = f
	}
	m.SetStructuredMut(s)
	return service.MessageBatch{m}, nil
}

func (o *ollamaEmbeddingProcessor) computeText(msg *service.Message) (string, error) {
	if o.text != nil {
		return o.text.TryString(msg)
	}
	b, err := msg.AsBytes()
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("message payload contained invalid UTF8")
	}
	return string(b), nil
}

func (o *ollamaEmbeddingProcessor) generateEmbedding(ctx context.Context, text string) ([]float64, error) {
	var req api.EmbeddingRequest
	req.Model = o.model
	req.Prompt = text
	resp, err := o.client.Embeddings(ctx, &req)
	if err != nil {
		return nil, err
	}
	return resp.Embedding, nil
}

func (o *ollamaEmbeddingProcessor) Close(ctx context.Context) error {
	return o.baseOllamaProcessor.Close(ctx)
}

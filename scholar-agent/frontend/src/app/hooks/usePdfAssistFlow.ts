import { useCallback, useState } from 'react';
import { defaultLayoutPlugin } from '@react-pdf-viewer/default-layout';
import { useAITranslationPlugin } from '../../hooks/useAITranslationPlugin';
import type { ChatMessage } from '../../contracts/api';

interface UsePdfAssistFlowOptions {
  setPrompt: (value: string) => void;
  appendChatMessage: (message: ChatMessage) => void;
  appendSelectedTaskLog: (line: string) => void;
}

export function usePdfAssistFlow(options: UsePdfAssistFlowOptions) {
  const { setPrompt, appendChatMessage, appendSelectedTaskLog } = options;
  const [pdfUrl, setPdfUrl] = useState<string | null>(null);
  const isFullTranslating = false;
  const [showSuggestions, setShowSuggestions] = useState(true);
  const defaultLayoutPluginInstance = defaultLayoutPlugin();

  const handleAskAI = useCallback(
    (selectedText: string) => {
      setPrompt(`请帮我详细解释这篇文献中的这段内容：\n"${selectedText}"`);
      setPdfUrl(null);
      appendSelectedTaskLog('[System] 已获取划词内容，准备向 ScholarAgent 发起追问...');
    },
    [appendSelectedTaskLog, setPrompt],
  );

  const aiTranslationPluginInstance = useAITranslationPlugin(handleAskAI);

  const handleFullTranslation = useCallback(() => {
    appendChatMessage({
      role: 'system',
      text: '全文翻译服务尚未接入，当前 PDF 不会被翻译。你可以复制需要翻译的段落，在对话中提出翻译请求。',
    });
  }, [appendChatMessage]);

  return {
    pdfUrl,
    setPdfUrl,
    isFullTranslating,
    showSuggestions,
    setShowSuggestions,
    defaultLayoutPluginInstance,
    aiTranslationPluginInstance,
    handleFullTranslation,
  };
}

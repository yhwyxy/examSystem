# 题目：找出元素item在给定数组array中的位置

## 问题分析
给定一个数组和一个元素，找出该元素在数组中的位置（索引，从0开始）。

## 示例
输入：`[1, 2, 3, 4], 3`
输出：`2`

## 可能的实现方式

### 1. JavaScript实现

#### 方法1：使用内置indexOf方法
```javascript
function findIndex(array, item) {
    return array.indexOf(item);
}
```

#### 方法2：使用findIndex方法
```javascript
function findIndex(array, item) {
    return array.findIndex(element => element === item);
}
```

#### 方法3：手动循环
```javascript
function findIndex(array, item) {
    for (let i = 0; i < array.length; i++) {
        if (array[i] === item) {
            return i;
        }
    }
    return -1; // 未找到
}
```

### 2. Java实现

#### 方法1：手动循环
```java
public static int findIndex(int[] array, int item) {
    for (int i = 0; i < array.length; i++) {
        if (array[i] == item) {
            return i;
        }
    }
    return -1;
}
```

#### 方法2：使用Arrays.asList（适用于对象数组）
```java
import java.util.Arrays;

public static int findIndex(Integer[] array, int item) {
    return Arrays.asList(array).indexOf(item);
}
```

### 3. C语言实现

#### 方法：手动循环
```c
#include <stdio.h>

int findIndex(int array[], int size, int item) {
    for (int i = 0; i < size; i++) {
        if (array[i] == item) {
            return i;
        }
    }
    return -1;
}
```

### 4. C#实现

#### 方法1：使用Array.IndexOf
```csharp
public static int FindIndex(int[] array, int item) {
    return Array.IndexOf(array, item);
}
```

#### 方法2：手动循环
```csharp
public static int FindIndex(int[] array, int item) {
    for (int i = 0; i < array.Length; i++) {
        if (array[i] == item) {
            return i;
        }
    }
    return -1;
}
```

### 5. Python实现

#### 方法1：使用list.index方法
```python
def find_index(array, item):
    return array.index(item)
```

#### 方法2：手动循环
```python
def find_index(array, item):
    for i, element in enumerate(array):
        if element == item:
            return i
    return -1
```

#### 方法3：使用列表推导式
```python
def find_index(array, item):
    indices = [i for i, x in enumerate(array) if x == item]
    return indices[0] if indices else -1
```

## 答案数量
每种语言都有多种实现方式：
- JavaScript：3种主要方法（indexOf、findIndex、手动循环）
- Java：2种主要方法（手动循环、Arrays.asList）
- C：1种方法（手动循环，因为没有内置方法）
- C#：2种主要方法（Array.IndexOf、手动循环）
- Python：3种主要方法（list.index、手动循环、列表推导式）

总计：5种语言，11种不同的实现方法。

## 推荐实现
根据题目要求，选择JavaScript实现，使用最简单直接的方法：

```javascript
function findIndex(array, item) {
    return array.indexOf(item);
}

// 测试
const array = [1, 2, 3, 4];
const item = 3;
console.log(findIndex(array, item)); // 输出：2
```
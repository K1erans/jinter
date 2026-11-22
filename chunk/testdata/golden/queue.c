#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define QUEUE_MAX 64

/* A fixed-size ring buffer of ints. */
typedef struct {
    int items[QUEUE_MAX];
    int head;
    int len;
} queue;

static int full(const queue *q) {
    return q->len == QUEUE_MAX;
}

int queue_push(queue *q, int v) {
    if (full(q)) {
        return -1;
    }
    q->items[(q->head + q->len) % QUEUE_MAX] = v;
    q->len++;
    return 0;
}

int queue_pop(queue *q, int *out) {
    if (q->len == 0) {
        return -1;
    }
    *out = q->items[q->head];
    q->head = (q->head + 1) % QUEUE_MAX;
    q->len--;
    return 0;
}

struct node {
    char *name;
    struct node *next;
};

struct node *list_add(struct node *head, const char *name) {
    struct node *n = malloc(sizeof *n);
    if (!n) {
        return head;
    }
    n->name = strdup(name);
    n->next = head;
    return n;
}

int main(int argc, char **argv) {
    queue q = {0};
    for (int i = 1; i < argc; i++) {
        queue_push(&q, atoi(argv[i]));
    }
    int v;
    while (queue_pop(&q, &v) == 0) {
        printf("%d\n", v);
    }
    return 0;
}
